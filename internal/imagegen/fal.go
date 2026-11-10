package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// fal talks to the fal.ai queue API:
//
//	POST {base}/{model}            -> {request_id, status_url, response_url}
//	GET  status_url                -> {status: IN_QUEUE|IN_PROGRESS|COMPLETED}
//	GET  response_url              -> model output ({images:[{url,...}], seed})
//
// Reference images are sent inline as base64 data URIs, which fal accepts
// for any file input, so no separate upload step is needed.
type fal struct {
	http    *http.Client
	baseURL string
	apiKey  string
	headers map[string]string
	poll    time.Duration
}

func newFal(o Options) *fal {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	base := strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	if base == "" {
		base = "https://queue.fal.run"
	}
	poll := o.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	return &fal{http: hc, baseURL: base, apiKey: o.APIKey, headers: o.Headers, poll: poll}
}

// falImageSize maps an aspect ratio to fal's image_size enum used by FLUX
// text-to-image endpoints.
var falImageSize = map[string]string{
	"9:16": "portrait_16_9",
	"16:9": "landscape_16_9",
	"3:4":  "portrait_4_3",
	"4:3":  "landscape_4_3",
	"1:1":  "square_hd",
}

// payload builds the model input. Different fal endpoints name their inputs
// differently (image_size vs aspect_ratio, image_url vs image_urls); we send
// the common variants and fal ignores fields a model does not declare.
// Anything can be overridden through Request.Extra.
func (f *fal) payload(req *Request) map[string]any {
	p := map[string]any{"prompt": req.Prompt}
	if req.NegativePrompt != "" {
		p["negative_prompt"] = req.NegativePrompt
	}
	if req.AspectRatio != "" {
		p["aspect_ratio"] = req.AspectRatio
		if s, ok := falImageSize[req.AspectRatio]; ok {
			p["image_size"] = s
		}
	}
	if req.NumImages > 0 {
		p["num_images"] = req.NumImages
	}
	if req.Seed != nil {
		p["seed"] = *req.Seed
	}
	if len(req.References) > 0 {
		urls := make([]string, 0, len(req.References))
		for _, img := range req.References {
			if img.URL != "" {
				urls = append(urls, img.URL)
			} else {
				urls = append(urls, "data:"+img.MIME+";base64,"+base64.StdEncoding.EncodeToString(img.Data))
			}
		}
		p["image_url"] = urls[0]
		p["image_urls"] = urls
	}
	for k, v := range req.Extra {
		p[k] = v
	}
	return p
}

// DescribePayload implements PayloadDescriber.
func (f *fal) DescribePayload(req *Request) map[string]any {
	return redactData(f.payload(req)).(map[string]any)
}

// redactData replaces data URIs anywhere in v with "data:<type>;base64,<n bytes>".
func redactData(v any) any {
	switch x := v.(type) {
	case string:
		if rest, ok := strings.CutPrefix(x, "data:"); ok {
			meta, payload, _ := strings.Cut(rest, ",")
			return fmt.Sprintf("data:%s,<%d bytes>", meta, base64.StdEncoding.DecodedLen(len(payload)))
		}
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = redactData(s)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactData(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = redactData(e)
		}
		return out
	}
	return v
}

func (f *fal) do(ctx context.Context, method, url string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f.apiKey != "" {
		req.Header.Set("Authorization", "Key "+f.apiKey)
	}
	for k, v := range f.headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		return fmt.Errorf("fal: HTTP %d: %s", resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	return json.Unmarshal(data, out)
}

func (f *fal) Generate(ctx context.Context, req *Request) (*Result, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("fal: model is required")
	}
	var sub struct {
		RequestID   string `json:"request_id"`
		StatusURL   string `json:"status_url"`
		ResponseURL string `json:"response_url"`
	}
	if err := f.do(ctx, http.MethodPost, f.baseURL+"/"+strings.Trim(req.Model, "/"), f.payload(req), &sub); err != nil {
		return nil, err
	}
	if sub.StatusURL == "" || sub.ResponseURL == "" {
		return nil, fmt.Errorf("fal: queue response missing status/response URL")
	}
	for {
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := f.do(ctx, http.MethodGet, sub.StatusURL, nil, &st); err != nil {
			return nil, err
		}
		if st.Status == "COMPLETED" {
			if st.Error != "" {
				return nil, fmt.Errorf("fal: %s", st.Error)
			}
			break
		}
		if st.Status != "IN_QUEUE" && st.Status != "IN_PROGRESS" {
			return nil, fmt.Errorf("fal: unexpected status %q %s", st.Status, st.Error)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.poll):
		}
	}
	var raw json.RawMessage
	if err := f.do(ctx, http.MethodGet, sub.ResponseURL, nil, &raw); err != nil {
		return nil, err
	}
	var out struct {
		Images []struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
		} `json:"images"`
		Image *struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
		} `json:"image"`
		Seed *int64 `json:"seed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("fal: decode result: %w", err)
	}
	if out.Image != nil {
		out.Images = append(out.Images, *out.Image)
	}
	if len(out.Images) == 0 {
		return nil, fmt.Errorf("fal: result has no images: %s", truncate(string(raw), 500))
	}
	res := &Result{Seed: out.Seed, RequestID: sub.RequestID, Raw: raw}
	for _, im := range out.Images {
		data, ct, err := f.fetch(ctx, im.URL)
		if err != nil {
			return nil, err
		}
		if im.ContentType != "" {
			ct = im.ContentType
		}
		res.Images = append(res.Images, Image{MIME: ct, Data: data, URL: im.URL, Width: im.Width, Height: im.Height})
	}
	return res, nil
}

// fetch downloads an output image (https URL or data URI).
func (f *fal) fetch(ctx context.Context, url string) ([]byte, string, error) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, payload, ok := strings.Cut(rest, ",")
		if !ok {
			return nil, "", fmt.Errorf("fal: malformed data URI")
		}
		mt, _, _ := strings.Cut(meta, ";")
		if !strings.HasSuffix(meta, ";base64") {
			return []byte(payload), mt, nil
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		return data, mt, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("fal: download %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	if ct == "" || ct == "application/octet-stream" {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

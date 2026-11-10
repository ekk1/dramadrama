package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
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
	// pagePause is the delay between pages when listing models.
	pagePause time.Duration
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
	return &fal{http: hc, baseURL: base, apiKey: o.APIKey, headers: o.Headers, poll: poll, pagePause: poll / 10}
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

// payload builds the model input.
func (f *fal) payload(req *Request) map[string]any {
	if req.Kind == KindVideo {
		p, _ := videoPayload(req)
		return p
	}
	return imagePayload(req)
}

// imagePayload builds an image model's input. Different fal endpoints
// name their inputs differently (image_size vs aspect_ratio, image_url vs
// image_urls); we send the common variants and fal ignores fields a model
// does not declare. Anything can be overridden through Request.Extra.
func imagePayload(req *Request) map[string]any {
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
		urls := fileURLs(req.References)
		p["image_url"] = urls[0]
		p["image_urls"] = urls
	}
	for k, v := range req.Extra {
		p[k] = v
	}
	return p
}

// fileURL returns the value sent for a file input: its public URL, or the
// content inline as a base64 data URI, which fal accepts for any file
// input.
func fileURL(img Image) string {
	if img.URL != "" {
		return img.URL
	}
	return "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

func fileURLs(list []Image) []string {
	urls := make([]string, len(list))
	for i, img := range list {
		urls[i] = fileURL(img)
	}
	return urls
}

// roleNames are the display names used in Explain.
var roleNames = map[string]string{
	RoleFirstFrame: "first frame", RoleLastFrame: "last frame", RoleRefImages: "reference images",
	RoleRefVideos: "reference videos", RoleRefAudios: "reference audio", RoleDuration: "duration", RoleAspect: "aspect ratio",
}

// videoPayload builds a video model's input by matching the request's
// inputs to the model's fields (see ResolveRoles). The notes say what
// went where and what could not be sent.
func videoPayload(req *Request) (map[string]any, []string) {
	roles := ResolveRoles(req.Schema, req.Mapping)
	p := map[string]any{}
	var notes []string
	if req.Schema == nil {
		notes = append(notes, "The model's parameter list is unknown: only the prompt and the first frame are sent.")
	}
	if pr, ok := roles[RolePrompt]; ok {
		p[pr.Name] = req.Prompt
	}
	// files assigns a list of inputs to the field that plays a role.
	files := func(role string, list []Image) {
		if len(list) == 0 {
			return
		}
		pr, ok := roles[role]
		if !ok {
			notes = append(notes, roleNames[role]+": not supported by this model, ignored")
			return
		}
		note := roleNames[role] + " → " + pr.Name
		if pr.Type == "array" {
			p[pr.Name] = fileURLs(list)
		} else {
			p[pr.Name] = fileURL(list[0])
			if len(list) > 1 {
				note += fmt.Sprintf(" (only the first of %d)", len(list))
			}
		}
		notes = append(notes, note)
	}
	one := func(img *Image) []Image {
		if img == nil {
			return nil
		}
		return []Image{*img}
	}
	files(RoleFirstFrame, one(req.FirstFrame))
	files(RoleLastFrame, one(req.LastFrame))
	files(RoleRefImages, req.References)
	files(RoleRefVideos, req.RefVideos)
	files(RoleRefAudios, req.RefAudios)

	if req.Duration > 0 {
		if pr, ok := roles[RoleDuration]; ok {
			if v := durationValue(pr, req.Duration); v != nil {
				p[pr.Name] = v
				notes = append(notes, fmt.Sprintf("duration → %s = %v", pr.Name, v))
			}
		}
	}
	if req.AspectRatio != "" {
		if pr, ok := roles[RoleAspect]; ok {
			if len(pr.Enum) == 0 || slices.Contains(pr.Enum, req.AspectRatio) {
				p[pr.Name] = req.AspectRatio
			} else {
				notes = append(notes, fmt.Sprintf("aspect ratio: %s is not offered by this model, its default is used", req.AspectRatio))
			}
		}
	}
	if req.Seed != nil {
		p["seed"] = *req.Seed
	}
	for k, v := range req.Extra {
		p[k] = v
	}
	return p, notes
}

// Explain implements Explainer.
func (f *fal) Explain(req *Request) []string {
	if req.Kind != KindVideo {
		return nil
	}
	_, notes := videoPayload(req)
	return notes
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

// HTTPError is a non-2xx response from the provider.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("fal: HTTP %d: %s", e.StatusCode, e.Body) }

// Retryable reports whether err leaves the outcome of a job unknown, so
// that waiting for it again later may still succeed: cancellation,
// network failures, and overload or server errors. Any other error is the
// provider's final word on the job.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode >= 500 || he.StatusCode == http.StatusTooManyRequests
	}
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &ne) || errors.As(err, &ue)
}

func (f *fal) do(ctx context.Context, method, target string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
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
		return &HTTPError{StatusCode: resp.StatusCode, Body: msg}
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

// Generate submits a request and waits for its result.
func (f *fal) Generate(ctx context.Context, req *Request) (*Result, error) {
	job, err := f.Submit(ctx, req)
	if err != nil {
		return nil, err
	}
	return f.Wait(ctx, job, nil)
}

// Submit implements Queued.
func (f *fal) Submit(ctx context.Context, req *Request) (*Job, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("fal: model is required")
	}
	var job Job
	if err := f.do(ctx, http.MethodPost, f.baseURL+"/"+strings.Trim(req.Model, "/"), f.payload(req), &job); err != nil {
		return nil, err
	}
	if job.StatusURL == "" || job.ResponseURL == "" {
		return nil, fmt.Errorf("fal: queue response missing status/response URL")
	}
	return &job, nil
}

// Cancel implements Queued: PUT {status_url minus "/status"}/cancel.
func (f *fal) Cancel(ctx context.Context, job *Job) error {
	base, ok := strings.CutSuffix(job.StatusURL, "/status")
	if !ok {
		return fmt.Errorf("fal: cannot derive the cancel URL")
	}
	return f.do(ctx, http.MethodPut, base+"/cancel", nil, nil)
}

type falFile struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// Wait implements Queued.
func (f *fal) Wait(ctx context.Context, job *Job, onStatus func(string)) (*Result, error) {
	last := ""
	for n := 0; ; n++ {
		var st struct {
			Status   string `json:"status"`
			Error    string `json:"error"`
			Position *int   `json:"queue_position"`
		}
		if err := f.do(ctx, http.MethodGet, job.StatusURL, nil, &st); err != nil {
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
		if desc := st.Status; onStatus != nil {
			if st.Status == "IN_QUEUE" && st.Position != nil {
				desc = fmt.Sprintf("IN_QUEUE (position %d)", *st.Position)
			}
			if desc != last {
				onStatus(desc)
				last = desc
			}
		}
		// Poll quickly at first (images take seconds), then back off for
		// long jobs (video takes minutes).
		wait := f.poll * time.Duration(min(1+n/15, 5))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	var raw json.RawMessage
	if err := f.do(ctx, http.MethodGet, job.ResponseURL, nil, &raw); err != nil {
		return nil, err
	}
	var out struct {
		Images []falFile `json:"images"`
		Image  *falFile  `json:"image"`
		Videos []falFile `json:"videos"`
		Video  *falFile  `json:"video"`
		Seed   *int64    `json:"seed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("fal: decode result: %w", err)
	}
	if out.Image != nil {
		out.Images = append(out.Images, *out.Image)
	}
	if out.Video != nil {
		out.Videos = append(out.Videos, *out.Video)
	}
	if len(out.Images) == 0 && len(out.Videos) == 0 {
		return nil, fmt.Errorf("fal: result has no images or video: %s", truncate(string(raw), 500))
	}
	res := &Result{Seed: out.Seed, RequestID: job.RequestID, Raw: raw}
	download := func(files []falFile) ([]Image, error) {
		var list []Image
		for _, im := range files {
			data, ct, err := f.fetch(ctx, im.URL)
			if err != nil {
				return nil, err
			}
			if im.ContentType != "" {
				ct = im.ContentType
			}
			list = append(list, Image{MIME: ct, Data: data, URL: im.URL, Width: im.Width, Height: im.Height})
		}
		return list, nil
	}
	var err error
	if res.Images, err = download(out.Images); err != nil {
		return nil, err
	}
	if res.Videos, err = download(out.Videos); err != nil {
		return nil, err
	}
	return res, nil
}

// maxDownload caps one output file; video clips can be large.
const maxDownload = 512 << 20

// fetch downloads an output file (https URL or data URI).
func (f *fal) fetch(ctx context.Context, src string) ([]byte, string, error) {
	if rest, ok := strings.CutPrefix(src, "data:"); ok {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", &HTTPError{StatusCode: resp.StatusCode, Body: "download " + src}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
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

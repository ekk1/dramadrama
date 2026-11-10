package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ModelInfo describes one model offered by a provider.
type ModelInfo struct {
	ID          string
	Name        string // display name, may be empty
	Description string
}

// maxModelPages bounds pagination when listing models.
const maxModelPages = 50

// ListModels asks the provider which models it offers.
func ListModels(ctx context.Context, o Options) ([]ModelInfo, error) {
	c, err := New(o)
	if err != nil {
		return nil, err
	}
	l, ok := c.(interface {
		listModels(ctx context.Context) ([]ModelInfo, error)
	})
	if !ok {
		return nil, fmt.Errorf("llm: provider type %q cannot list models", o.Type)
	}
	list, err := l.listModels(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	return list, nil
}

// getJSON sends a GET request and decodes the JSON response into out.
func (b *base) getJSON(ctx context.Context, url string, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
}

// OpenAI and compatible servers: GET {base}/models -> {data: [{id}]}.
func openAIListModels(ctx context.Context, b *base, hdr map[string]string) ([]ModelInfo, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := b.getJSON(ctx, b.baseURL+"/models", hdr, &out); err != nil {
		return nil, err
	}
	list := make([]ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			list = append(list, ModelInfo{ID: m.ID})
		}
	}
	return list, nil
}

func (c *openAIChat) listModels(ctx context.Context) ([]ModelInfo, error) {
	return openAIListModels(ctx, &c.base, c.headers())
}

func (c *openAIResponses) listModels(ctx context.Context) ([]ModelInfo, error) {
	return openAIListModels(ctx, &c.base, c.headers())
}

// Anthropic: GET {base}/v1/models?limit=…&after_id=… ->
// {data: [{id, display_name}], has_more, last_id}.
func (c *anthropic) listModels(ctx context.Context) ([]ModelInfo, error) {
	var list []ModelInfo
	after := ""
	for range maxModelPages {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var out struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := c.getJSON(ctx, c.baseURL+"/v1/models?"+q.Encode(), c.headers(), &out); err != nil {
			return nil, err
		}
		for _, m := range out.Data {
			list = append(list, ModelInfo{ID: m.ID, Name: m.DisplayName})
		}
		if !out.HasMore || out.LastID == "" || out.LastID == after {
			break
		}
		after = out.LastID
	}
	return list, nil
}

// Gemini: GET {base}/models?pageSize=…&pageToken=… ->
// {models: [{name: "models/…", displayName, description,
// supportedGenerationMethods}], nextPageToken}. Only models that support
// generateContent are returned.
func (c *gemini) listModels(ctx context.Context) ([]ModelInfo, error) {
	var list []ModelInfo
	token := ""
	for range maxModelPages {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var out struct {
			Models []struct {
				Name        string   `json:"name"`
				DisplayName string   `json:"displayName"`
				Description string   `json:"description"`
				Methods     []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.getJSON(ctx, c.baseURL+"/models?"+q.Encode(), c.headers(), &out); err != nil {
			return nil, err
		}
		for _, m := range out.Models {
			if len(m.Methods) > 0 && !slices.Contains(m.Methods, "generateContent") {
				continue
			}
			list = append(list, ModelInfo{ID: strings.TrimPrefix(m.Name, "models/"), Name: m.DisplayName, Description: m.Description})
		}
		if out.NextPageToken == "" || out.NextPageToken == token {
			break
		}
		token = out.NextPageToken
	}
	return list, nil
}

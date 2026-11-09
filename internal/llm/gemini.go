package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/url"
	"strings"
)

// gemini implements the Google Gemini API
// (POST {base}/models/{model}:generateContent and :streamGenerateContent).
type gemini struct{ base }

type gmContent struct {
	Role  string   `json:"role,omitempty"`
	Parts []gmPart `json:"parts"`
}

type gmPart struct {
	Text       string        `json:"text,omitempty"`
	InlineData *gmInlineData `json:"inlineData,omitempty"`
	Thought    bool          `json:"thought,omitempty"`
}

type gmInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type gmRequest struct {
	SystemInstruction *gmContent     `json:"systemInstruction,omitempty"`
	Contents          []gmContent    `json:"contents"`
	GenerationConfig  map[string]any `json:"generationConfig,omitempty"`
}

type gmResponse struct {
	Candidates []struct {
		Content      gmContent `json:"content"`
		FinishReason string    `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (r *gmResponse) text() string {
	var sb strings.Builder
	if len(r.Candidates) > 0 {
		for _, p := range r.Candidates[0].Content.Parts {
			if !p.Thought {
				sb.WriteString(p.Text)
			}
		}
	}
	return sb.String()
}

func (r *gmResponse) finish() string {
	if len(r.Candidates) > 0 {
		return r.Candidates[0].FinishReason
	}
	return ""
}

func (r *gmResponse) usage() *Usage {
	if r.UsageMetadata == nil {
		return nil
	}
	return &Usage{InputTokens: r.UsageMetadata.PromptTokenCount, OutputTokens: r.UsageMetadata.CandidatesTokenCount}
}

func (r *gmResponse) err() error {
	if r.Error != nil {
		return fmt.Errorf("llm: gemini: %s", r.Error.Message)
	}
	if r.PromptFeedback != nil && r.PromptFeedback.BlockReason != "" {
		return fmt.Errorf("llm: gemini: prompt blocked: %s", r.PromptFeedback.BlockReason)
	}
	return nil
}

func (c *gemini) build(req *Request) *gmRequest {
	out := &gmRequest{}
	if req.System != "" {
		out.SystemInstruction = &gmContent{Parts: []gmPart{{Text: req.System}}}
	}
	for _, m := range req.Messages {
		role := "user"
		if m.Role == RoleAssistant {
			role = "model"
		}
		gc := gmContent{Role: role}
		for _, p := range m.Content {
			switch p.Type {
			case PartText:
				gc.Parts = append(gc.Parts, gmPart{Text: p.Text})
			case PartImage:
				gc.Parts = append(gc.Parts, gmPart{InlineData: &gmInlineData{
					MimeType: p.MIME, Data: base64.StdEncoding.EncodeToString(p.Data),
				}})
			}
		}
		out.Contents = append(out.Contents, gc)
	}
	gen := map[string]any{}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.JSON {
		gen["responseMimeType"] = "application/json"
	}
	if len(gen) > 0 {
		out.GenerationConfig = gen
	}
	return out
}

func (c *gemini) headers() map[string]string {
	h := map[string]string{}
	if c.apiKey != "" {
		h["x-goog-api-key"] = c.apiKey
	}
	return h
}

func (c *gemini) url(model, method string) string {
	model = strings.TrimPrefix(model, "models/")
	return c.baseURL + "/models/" + url.PathEscape(model) + ":" + method
}

func (c *gemini) Chat(ctx context.Context, req *Request) (*Response, error) {
	var out gmResponse
	if err := c.postJSON(ctx, c.url(req.Model, "generateContent"), c.headers(), c.build(req), &out); err != nil {
		return nil, err
	}
	if err := out.err(); err != nil {
		return nil, err
	}
	resp := &Response{Text: out.text(), FinishReason: out.finish()}
	if u := out.usage(); u != nil {
		resp.Usage = *u
	}
	return resp, nil
}

func (c *gemini) Stream(ctx context.Context, req *Request) iter.Seq2[Chunk, error] {
	return c.streamSSE(ctx, c.url(req.Model, "streamGenerateContent")+"?alt=sse", c.headers(), c.build(req),
		func(ev sseEvent, yield func(Chunk) bool) (bool, error) {
			if ev.Data == "" {
				return true, nil
			}
			var d gmResponse
			if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
				return false, fmt.Errorf("llm: gemini: bad stream event: %w", err)
			}
			if err := d.err(); err != nil {
				return false, err
			}
			ch := Chunk{Delta: d.text(), FinishReason: d.finish(), Usage: d.usage()}
			if ch.Delta == "" && ch.FinishReason == "" && ch.Usage == nil {
				return true, nil
			}
			return yield(ch), nil
		})
}

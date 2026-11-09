package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
)

// openAIResponses implements the OpenAI Responses API (POST {base}/responses).
type openAIResponses struct{ base }

type oaRespInput struct {
	Role    string       `json:"role"`
	Content []oaRespPart `json:"content"`
}

type oaRespPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type oaRespRequest struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []oaRespInput  `json:"input"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	Store           bool           `json:"store"`
	Text            map[string]any `json:"text,omitempty"`
}

type oaRespUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// oaRespObject is the subset of the Response object we read.
type oaRespObject struct {
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *oaRespUsage `json:"usage"`
}

func (o *oaRespObject) finishReason() string {
	if o.IncompleteDetails != nil && o.IncompleteDetails.Reason != "" {
		return o.IncompleteDetails.Reason
	}
	return o.Status
}

func (o *oaRespObject) usage() *Usage {
	if o.Usage == nil {
		return nil
	}
	return &Usage{InputTokens: o.Usage.InputTokens, OutputTokens: o.Usage.OutputTokens}
}

func (c *openAIResponses) build(req *Request, stream bool) *oaRespRequest {
	out := &oaRespRequest{
		Model:           req.Model,
		Instructions:    req.System,
		Temperature:     req.Temperature,
		MaxOutputTokens: req.MaxTokens,
		Stream:          stream,
	}
	if req.JSON {
		out.Text = map[string]any{"format": map[string]any{"type": "json_object"}}
	}
	for _, m := range req.Messages {
		in := oaRespInput{Role: string(m.Role)}
		for _, p := range m.Content {
			switch {
			case p.Type == PartText && m.Role == RoleAssistant:
				in.Content = append(in.Content, oaRespPart{Type: "output_text", Text: p.Text})
			case p.Type == PartText:
				in.Content = append(in.Content, oaRespPart{Type: "input_text", Text: p.Text})
			case p.Type == PartImage:
				in.Content = append(in.Content, oaRespPart{Type: "input_image", ImageURL: dataURI(p)})
			}
		}
		out.Input = append(out.Input, in)
	}
	return out
}

func (c *openAIResponses) headers() map[string]string {
	h := map[string]string{}
	if c.apiKey != "" {
		h["Authorization"] = "Bearer " + c.apiKey
	}
	return h
}

func (c *openAIResponses) Chat(ctx context.Context, req *Request) (*Response, error) {
	var out oaRespObject
	if err := c.postJSON(ctx, c.baseURL+"/responses", c.headers(), c.build(req, false), &out); err != nil {
		return nil, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, fmt.Errorf("llm: openai responses: %s", out.Error.Message)
	}
	var sb strings.Builder
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				sb.WriteString(c.Text)
			}
		}
	}
	resp := &Response{Text: sb.String(), FinishReason: out.finishReason()}
	if u := out.usage(); u != nil {
		resp.Usage = *u
	}
	return resp, nil
}

func (c *openAIResponses) Stream(ctx context.Context, req *Request) iter.Seq2[Chunk, error] {
	return c.streamSSE(ctx, c.baseURL+"/responses", c.headers(), c.build(req, true),
		func(ev sseEvent, yield func(Chunk) bool) (bool, error) {
			if ev.Data == "" || ev.Data == "[DONE]" {
				return ev.Data != "[DONE]", nil
			}
			var d struct {
				Type     string        `json:"type"`
				Delta    string        `json:"delta"`
				Message  string        `json:"message"`
				Response *oaRespObject `json:"response"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
				return false, fmt.Errorf("llm: openai responses: bad stream event: %w", err)
			}
			switch d.Type {
			case "response.output_text.delta":
				return yield(Chunk{Delta: d.Delta}), nil
			case "response.completed", "response.incomplete":
				ch := Chunk{FinishReason: "completed"}
				if d.Response != nil {
					ch.FinishReason = d.Response.finishReason()
					ch.Usage = d.Response.usage()
				}
				yield(ch)
				return false, nil
			case "response.failed":
				msg := "response failed"
				if d.Response != nil && d.Response.Error != nil {
					msg = d.Response.Error.Message
				}
				return false, fmt.Errorf("llm: openai responses: %s", msg)
			case "error":
				return false, fmt.Errorf("llm: openai responses: %s", d.Message)
			}
			return true, nil
		})
}

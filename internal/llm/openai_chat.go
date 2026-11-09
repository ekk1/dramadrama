package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
)

// openAIChat implements the OpenAI Chat Completions API
// (POST {base}/chat/completions). Most OpenAI-compatible servers
// (DeepSeek, Qwen, OpenRouter, vLLM, Ollama, ...) speak this protocol.
type openAIChat struct{ base }

type oaChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []oaChatPart
}

type oaChatPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *oaChatImgURL `json:"image_url,omitempty"`
}

type oaChatImgURL struct {
	URL string `json:"url"`
}

type oaChatRequest struct {
	Model          string          `json:"model"`
	Messages       []oaChatMessage `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	StreamOptions  map[string]any  `json:"stream_options,omitempty"`
	ResponseFormat map[string]any  `json:"response_format,omitempty"`
}

type oaChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *oaChatUsage) usage() *Usage {
	if u == nil {
		return nil
	}
	return &Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

func (c *openAIChat) build(req *Request, stream bool) *oaChatRequest {
	out := &oaChatRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	if stream {
		out.StreamOptions = map[string]any{"include_usage": true}
	}
	if req.JSON {
		out.ResponseFormat = map[string]any{"type": "json_object"}
	}
	if req.System != "" {
		out.Messages = append(out.Messages, oaChatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, oaChatMessage{Role: string(m.Role), Content: oaChatContent(m.Content)})
	}
	return out
}

// oaChatContent uses the plain string form for text-only content (widest
// compatibility) and the parts form when images are present.
func oaChatContent(parts []Part) any {
	textOnly := true
	for _, p := range parts {
		if p.Type != PartText {
			textOnly = false
		}
	}
	if textOnly {
		s := ""
		for i, p := range parts {
			if i > 0 {
				s += "\n\n"
			}
			s += p.Text
		}
		return s
	}
	out := make([]oaChatPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case PartText:
			out = append(out, oaChatPart{Type: "text", Text: p.Text})
		case PartImage:
			out = append(out, oaChatPart{Type: "image_url", ImageURL: &oaChatImgURL{URL: dataURI(p)}})
		}
	}
	return out
}

func (c *openAIChat) headers() map[string]string {
	h := map[string]string{}
	if c.apiKey != "" {
		h["Authorization"] = "Bearer " + c.apiKey
	}
	return h
}

func (c *openAIChat) Chat(ctx context.Context, req *Request) (*Response, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaChatUsage `json:"usage"`
	}
	if err := c.postJSON(ctx, c.baseURL+"/chat/completions", c.headers(), c.build(req, false), &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm: openai chat: response has no choices")
	}
	resp := &Response{Text: out.Choices[0].Message.Content, FinishReason: out.Choices[0].FinishReason}
	if u := out.Usage.usage(); u != nil {
		resp.Usage = *u
	}
	return resp, nil
}

func (c *openAIChat) Stream(ctx context.Context, req *Request) iter.Seq2[Chunk, error] {
	return c.streamSSE(ctx, c.baseURL+"/chat/completions", c.headers(), c.build(req, true),
		func(ev sseEvent, yield func(Chunk) bool) (bool, error) {
			if ev.Data == "" {
				return true, nil
			}
			if ev.Data == "[DONE]" {
				return false, nil
			}
			var d struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *oaChatUsage `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
				return false, fmt.Errorf("llm: openai chat: bad stream event: %w", err)
			}
			if d.Error != nil {
				return false, fmt.Errorf("llm: openai chat: %s", d.Error.Message)
			}
			var ch Chunk
			if len(d.Choices) > 0 {
				ch.Delta = d.Choices[0].Delta.Content
				if d.Choices[0].FinishReason != nil {
					ch.FinishReason = *d.Choices[0].FinishReason
				}
			}
			ch.Usage = d.Usage.usage()
			if ch.Delta == "" && ch.FinishReason == "" && ch.Usage == nil {
				return true, nil
			}
			return yield(ch), nil
		})
}

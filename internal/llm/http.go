package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is returned when a provider responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 2000 {
		body = body[:2000] + "…"
	}
	return fmt.Sprintf("llm: HTTP %d: %s", e.StatusCode, body)
}

// base holds what every provider implementation shares.
type base struct {
	http    *http.Client
	baseURL string
	apiKey  string
	headers map[string]string
}

// post sends a JSON body and returns the response if the status is 2xx.
// The caller must close the body.
func (b *base) post(ctx context.Context, url string, hdr map[string]string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

// postJSON sends a request and decodes the JSON response into out.
func (b *base) postJSON(ctx context.Context, url string, hdr map[string]string, body, out any) error {
	resp, err := b.post(ctx, url, hdr, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("llm: decode response: %w", err)
	}
	return nil
}

// sseEvent is one Server-Sent Event.
type sseEvent struct {
	Event string
	Data  string
}

// errStop can be returned from an SSE handler to stop reading without error.
var errStop = errors.New("stop")

// readSSE parses a text/event-stream body and calls fn for each event.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var ev sseEvent
	var data []string
	dispatch := func() error {
		if len(data) == 0 && ev.Event == "" {
			return nil
		}
		ev.Data = strings.Join(data, "\n")
		err := fn(ev)
		ev, data = sseEvent{}, data[:0]
		return err
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if derr := dispatch(); derr != nil {
					if derr == errStop {
						return nil
					}
					return derr
				}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					ev.Event = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if derr := dispatch(); derr != nil && derr != errStop {
					return derr
				}
				return nil
			}
			return err
		}
	}
}

// streamSSE is the shared plumbing for streaming implementations: it opens
// the request and feeds each event to handle, which may yield chunks.
func (b *base) streamSSE(ctx context.Context, url string, hdr map[string]string, body any,
	handle func(ev sseEvent, yield func(Chunk) bool) (bool, error),
) func(yield func(Chunk, error) bool) {
	return func(yield func(Chunk, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		resp, err := b.post(ctx, url, hdr, body)
		if err != nil {
			yield(Chunk{}, err)
			return
		}
		defer resp.Body.Close()
		stopped := false
		y := func(c Chunk) bool {
			if !yield(c, nil) {
				stopped = true
				return false
			}
			return true
		}
		err = readSSE(resp.Body, func(ev sseEvent) error {
			cont, herr := handle(ev, y)
			if herr != nil {
				return herr
			}
			if !cont || stopped {
				return errStop
			}
			return nil
		})
		if err != nil && !stopped {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			yield(Chunk{}, err)
		}
	}
}

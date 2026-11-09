package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type captured struct {
	path   string
	query  string
	header http.Header
	body   map[string]any
}

// fakeServer returns body (JSON or SSE text) for any request and records it.
func fakeServer(t *testing.T, contentType, respBody string, cap *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		cap.path = r.URL.Path
		cap.query = r.URL.RawQuery
		cap.header = r.Header.Clone()
		cap.body = map[string]any{}
		if err := json.Unmarshal(data, &cap.body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testRequest() *Request {
	temp := 0.5
	return &Request{
		Model:       "m1",
		System:      "be brief",
		Temperature: &temp,
		Messages: []Message{{Role: RoleUser, Content: []Part{
			TextPart("hello"), ImagePart("image/png", []byte{1, 2, 3}),
		}}},
	}
}

func mustClient(t *testing.T, typ, url string) Client {
	t.Helper()
	c, err := New(Options{Type: typ, BaseURL: url, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func collect(t *testing.T, c Client) *Response {
	t.Helper()
	var deltas []string
	resp, err := Collect(c.Stream(context.Background(), testRequest()), func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(deltas) < 2 {
		t.Errorf("expected multiple deltas, got %q", deltas)
	}
	return resp
}

func TestOpenAIChat(t *testing.T) {
	var cap captured
	srv := fakeServer(t, "application/json",
		`{"choices":[{"message":{"content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`, &cap)
	c := mustClient(t, TypeOpenAIChat, srv.URL)
	resp, err := c.Chat(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hi there" || resp.FinishReason != "stop" || resp.Usage.OutputTokens != 2 {
		t.Errorf("unexpected response %+v", resp)
	}
	if cap.path != "/chat/completions" || cap.header.Get("Authorization") != "Bearer k" {
		t.Errorf("bad path/header: %s %v", cap.path, cap.header)
	}
	msgs := cap.body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system message missing: %v", msgs)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Errorf("bad image url %q", img)
	}
	if cap.body["temperature"] != 0.5 {
		t.Errorf("temperature not sent")
	}
}

func TestOpenAIChatStream(t *testing.T) {
	var cap captured
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		": keep-alive\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	srv := fakeServer(t, "text/event-stream", sse, &cap)
	resp := collect(t, mustClient(t, TypeOpenAIChat, srv.URL))
	if resp.Text != "Hello" || resp.FinishReason != "stop" || resp.Usage.InputTokens != 4 {
		t.Errorf("unexpected %+v", resp)
	}
	if cap.body["stream"] != true {
		t.Error("stream flag not set")
	}
}

func TestOpenAIResponses(t *testing.T) {
	var cap captured
	srv := fakeServer(t, "application/json",
		`{"status":"completed","output":[{"type":"reasoning","content":[]},{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":5,"output_tokens":1}}`, &cap)
	c := mustClient(t, TypeOpenAIResponses, srv.URL)
	resp, err := c.Chat(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "done" || resp.FinishReason != "completed" || resp.Usage.InputTokens != 5 {
		t.Errorf("unexpected %+v", resp)
	}
	if cap.path != "/responses" || cap.body["instructions"] != "be brief" {
		t.Errorf("bad request: %s %v", cap.path, cap.body)
	}
	in := cap.body["input"].([]any)[0].(map[string]any)["content"].([]any)
	if in[0].(map[string]any)["type"] != "input_text" || in[1].(map[string]any)["type"] != "input_image" {
		t.Errorf("bad input parts %v", in)
	}
}

func TestOpenAIResponsesStream(t *testing.T) {
	var cap captured
	sse := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"b\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
	srv := fakeServer(t, "text/event-stream", sse, &cap)
	resp := collect(t, mustClient(t, TypeOpenAIResponses, srv.URL))
	if resp.Text != "ab" || resp.FinishReason != "completed" || resp.Usage.OutputTokens != 2 {
		t.Errorf("unexpected %+v", resp)
	}
}

func TestAnthropic(t *testing.T) {
	var cap captured
	srv := fakeServer(t, "application/json",
		`{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"yo"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`, &cap)
	c := mustClient(t, TypeAnthropic, srv.URL)
	resp, err := c.Chat(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "yo" || resp.FinishReason != "end_turn" {
		t.Errorf("unexpected %+v", resp)
	}
	if cap.path != "/v1/messages" || cap.header.Get("x-api-key") != "k" || cap.header.Get("anthropic-version") != anthropicVersion {
		t.Errorf("bad request: %s %v", cap.path, cap.header)
	}
	if _, ok := cap.body["temperature"]; ok {
		t.Error("temperature must not be sent to Anthropic")
	}
	if cap.body["max_tokens"].(float64) <= 0 || cap.body["system"] != "be brief" {
		t.Errorf("bad body %v", cap.body)
	}
	src := cap.body["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Errorf("bad image source %v", src)
	}
}

func TestAnthropicStream(t *testing.T) {
	var cap captured
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	srv := fakeServer(t, "text/event-stream", sse, &cap)
	resp := collect(t, mustClient(t, TypeAnthropic, srv.URL))
	if resp.Text != "Hi!" || resp.FinishReason != "end_turn" || resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 3 {
		t.Errorf("unexpected %+v", resp)
	}
}

func TestGemini(t *testing.T) {
	var cap captured
	srv := fakeServer(t, "application/json",
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking","thought":true},{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}`, &cap)
	c := mustClient(t, TypeGemini, srv.URL)
	req := testRequest()
	req.JSON = true
	resp, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "ok" || resp.FinishReason != "STOP" {
		t.Errorf("unexpected %+v", resp)
	}
	if cap.path != "/models/m1:generateContent" || cap.header.Get("x-goog-api-key") != "k" {
		t.Errorf("bad request %s %v", cap.path, cap.header)
	}
	gen := cap.body["generationConfig"].(map[string]any)
	if gen["responseMimeType"] != "application/json" {
		t.Errorf("json mode not set: %v", gen)
	}
}

func TestGeminiStream(t *testing.T) {
	var cap captured
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Go\"}]}}]}\r\n\r\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"pher\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":2}}\r\n\r\n"
	srv := fakeServer(t, "text/event-stream", sse, &cap)
	resp := collect(t, mustClient(t, TypeGemini, srv.URL))
	if resp.Text != "Gopher" || resp.FinishReason != "STOP" {
		t.Errorf("unexpected %+v", resp)
	}
	if cap.path != "/models/m1:streamGenerateContent" || cap.query != "alt=sse" {
		t.Errorf("bad stream url %s?%s", cap.path, cap.query)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	for _, typ := range Types {
		c := mustClient(t, typ, srv.URL)
		_, err := c.Chat(context.Background(), testRequest())
		if ae, ok := err.(*APIError); !ok || ae.StatusCode != 401 {
			t.Errorf("%s: want APIError 401, got %v", typ, err)
		}
		_, err = Collect(c.Stream(context.Background(), testRequest()), nil)
		if ae, ok := err.(*APIError); !ok || ae.StatusCode != 401 {
			t.Errorf("%s stream: want APIError 401, got %v", typ, err)
		}
	}
}

func TestStreamEarlyBreak(t *testing.T) {
	var cap captured
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"
	srv := fakeServer(t, "text/event-stream", sse, &cap)
	c := mustClient(t, TypeOpenAIChat, srv.URL)
	n := 0
	for _, err := range c.Stream(context.Background(), testRequest()) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		break
	}
	if n != 1 {
		t.Errorf("got %d chunks", n)
	}
}

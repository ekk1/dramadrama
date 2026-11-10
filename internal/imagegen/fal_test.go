package imagegen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFalQueueFlow(t *testing.T) {
	var polls atomic.Int32
	var submitted map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/files/") && r.Header.Get("Authorization") != "Key secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/fal-ai/flux/dev":
			json.NewDecoder(r.Body).Decode(&submitted)
			fmt.Fprintf(w, `{"request_id":"r1","status_url":"%[1]s/fal-ai/flux/requests/r1/status","response_url":"%[1]s/fal-ai/flux/requests/r1"}`, srv.URL)
		case r.URL.Path == "/fal-ai/flux/requests/r1/status":
			if polls.Add(1) < 2 {
				w.Write([]byte(`{"status":"IN_PROGRESS"}`))
				return
			}
			w.Write([]byte(`{"status":"COMPLETED"}`))
		case r.URL.Path == "/fal-ai/flux/requests/r1":
			fmt.Fprintf(w, `{"images":[{"url":"%s/files/out.png","content_type":"image/png","width":720,"height":1280}],"seed":42}`, srv.URL)
		case r.URL.Path == "/files/out.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG fake"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g, err := New(Options{Type: TypeFal, BaseURL: srv.URL, APIKey: "secret", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.Generate(context.Background(), &Request{
		Model:       "fal-ai/flux/dev",
		Prompt:      "a cat",
		AspectRatio: "9:16",
		References:  []Image{{MIME: "image/jpeg", Data: []byte{1, 2}}},
		Extra:       map[string]any{"num_inference_steps": 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || string(res.Images[0].Data) != "\x89PNG fake" || res.Images[0].Width != 720 {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.Seed == nil || *res.Seed != 42 || res.RequestID != "r1" {
		t.Errorf("seed/request id not propagated: %+v", res)
	}
	if submitted["image_size"] != "portrait_16_9" || submitted["aspect_ratio"] != "9:16" {
		t.Errorf("size not mapped: %v", submitted)
	}
	if u, _ := submitted["image_url"].(string); !strings.HasPrefix(u, "data:image/jpeg;base64,") {
		t.Errorf("reference not sent as data uri: %v", submitted["image_url"])
	}
	if submitted["num_inference_steps"] != float64(20) {
		t.Errorf("extra not merged")
	}
}

func TestFalDataURIResult(t *testing.T) {
	f := newFal(Options{})
	data, mt, err := f.fetch(context.Background(), "data:image/png;base64,aGVsbG8=")
	if err != nil || string(data) != "hello" || mt != "image/png" {
		t.Fatalf("got %q %q %v", data, mt, err)
	}
}

func TestFalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"bad input"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	g, _ := New(Options{Type: TypeFal, BaseURL: srv.URL})
	_, err := g.Generate(context.Background(), &Request{Model: "x/y", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("expected 422 error, got %v", err)
	}
}

func TestFalDescribePayload(t *testing.T) {
	g, _ := New(Options{Type: TypeFal, APIKey: "secret"})
	d := g.(PayloadDescriber).DescribePayload(&Request{
		Model: "m", Prompt: "a cat", AspectRatio: "9:16",
		References: []Image{{MIME: "image/png", Data: make([]byte, 3000)}, {URL: "https://example.com/a.png"}},
	})
	b, _ := json.Marshal(d)
	got := string(b)
	for _, want := range []string{`"prompt":"a cat"`, `data:image/png;base64,\u003c3000 bytes\u003e`, `https://example.com/a.png`} {
		if !strings.Contains(got, want) {
			t.Errorf("payload %s missing %s", got, want)
		}
	}
	if strings.Contains(got, "secret") || len(got) > 600 {
		t.Errorf("payload not redacted: %d bytes", len(got))
	}
}

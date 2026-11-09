package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"iter"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/pipeline"
	"github.com/ekk1/dramadrama/internal/project"
)

type fakeLLM struct{}

func (fakeLLM) reply(req *llm.Request) string {
	switch {
	case strings.Contains(req.System, "art director"):
		return `{"style":"film look","items":[{"kind":"character","name":"Lin","description":"heroine","prompt":"woman in red"}]}`
	case strings.Contains(req.System, "storyboard director"):
		return `{"shots":[{"scene":"Shop","characters":["Lin"],"action":"opens <box>","camera":"close","duration":3}]}`
	case strings.Contains(req.System, "cinematographer"):
		return `{"storyboard_prompt":"sb","keyframe_prompt":"kf","motion_prompt":"mv"}`
	}
	return "A story about <Lin> & time."
}

func (f fakeLLM) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return &llm.Response{Text: f.reply(req)}, nil
}

func (f fakeLLM) Stream(ctx context.Context, req *llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{Delta: f.reply(req)}, nil) }
}

type fakeImage struct{}

func (fakeImage) Generate(ctx context.Context, req *imagegen.Request) (*imagegen.Result, error) {
	return &imagegen.Result{Images: []imagegen.Image{{MIME: "image/png", Data: []byte("png:" + req.Prompt)}}}, nil
}

type harness struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
	ps  *project.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "test") // providers are faked
	t.Setenv("FAL_KEY", "test")
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := project.Open(filepath.Join(dir, "projects"))
	lib, _ := assets.Open(filepath.Join(dir, "assets"))
	prompts := &pipeline.Prompts{OverrideDir: filepath.Join(dir, "prompts")}
	pipe := pipeline.New(cfg, ps, lib, prompts)
	pipe.NewLLM = func(*config.Provider) (llm.Client, error) { return fakeLLM{}, nil }
	pipe.NewImage = func(*config.Provider) (imagegen.Generator, error) { return fakeImage{}, nil }
	h, err := New(Options{Config: cfg, Projects: ps, Assets: lib, Pipeline: pipe, Prompts: prompts})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, srv: srv, c: &http.Client{Jar: jar}, ps: ps}
}

func (h *harness) get(path string) string {
	h.t.Helper()
	resp, err := h.c.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return string(body)
}

// post submits a form and returns the final URL after redirects.
func (h *harness) post(path string, form url.Values) *url.URL {
	h.t.Helper()
	resp, err := h.c.PostForm(h.srv.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("POST %s: %d %s", path, resp.StatusCode, body)
	}
	if e := resp.Request.URL.Query().Get("err"); e != "" {
		h.t.Fatalf("POST %s: error %s", path, e)
	}
	return resp.Request.URL
}

// run executes a step via SSE and returns the event stream text.
func (h *harness) run(pid, stage, target string) string {
	h.t.Helper()
	resp, err := h.c.PostForm(h.srv.URL+"/projects/"+pid+"/run/"+stage, url.Values{"target": {target}})
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		h.t.Fatalf("run %s: not an event stream: %s", stage, body)
	}
	if !strings.Contains(string(body), "event: done") {
		h.t.Fatalf("run %s did not finish: %s", stage, body)
	}
	return string(body)
}

func pngFile(t *testing.T) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	return buf.Bytes()
}

func TestWorkbenchFlow(t *testing.T) {
	h := newHarness(t)
	h.get("/projects")
	u := h.post("/projects", url.Values{"name": {"Demo"}, "idea": {"time machine"}})
	pid := strings.TrimPrefix(u.Path, "/projects/")
	if !strings.HasPrefix(pid, "p_") {
		t.Fatalf("unexpected redirect %s", u)
	}
	h.post("/projects/"+pid+"/input", url.Values{"name": {"Demo 2"}, "idea": {"a girl & a <clock>"}, "aspect_ratio": {"16:9"}})

	// Upload a reference image.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("target", "project")
	fw, _ := mw.CreateFormFile("files", "ref.png")
	fw.Write(pngFile(t))
	mw.Close()
	resp, err := h.c.Post(h.srv.URL+"/projects/"+pid+"/upload", mw.FormDataContentType(), &body)
	if err != nil || resp.StatusCode != 200 || resp.Request.URL.Query().Get("err") != "" {
		t.Fatalf("upload: %v %v", err, resp.Request.URL)
	}
	resp.Body.Close()

	stream := h.run(pid, "idea", "")
	if !strings.Contains(stream, "event: delta") || !strings.Contains(stream, "event: key") {
		t.Errorf("stream missing events: %s", stream)
	}
	h.run(pid, "visual_prompts", "")
	h.run(pid, "visual_images", "all")
	h.run(pid, "storyboard", "")
	h.run(pid, "shot_prompts", "all")
	h.run(pid, "keyframes", "all")

	p, _ := h.ps.Get(pid)
	if p.Name != "Demo 2" || p.AspectRatio != "16:9" || len(p.ReferenceIDs) != 1 || len(p.Shots) != 1 || len(p.Shots[0].KeyframeIDs) != 1 {
		t.Fatalf("unexpected project state: %+v", p)
	}

	page := h.get("/projects/" + pid)
	for _, want := range []string{"a girl &amp; a &lt;clock&gt;", "A story about &lt;Lin&gt;", "woman in red", "opens &lt;box&gt;", "/assets/" + p.Shots[0].KeyframeIDs[0] + "/file"} {
		if !strings.Contains(page, want) {
			t.Errorf("project page missing %q", want)
		}
	}

	// Shot edits only touch posted fields.
	sid := p.Shots[0].ID
	h.post("/projects/"+pid+"/shots/"+sid, url.Values{"keyframe_prompt": {"edited"}})
	p, _ = h.ps.Get(pid)
	if p.Shots[0].KeyframePrompt != "edited" || p.Shots[0].Scene != "Shop" {
		t.Errorf("partial shot update wrong: %+v", p.Shots[0])
	}

	// Attach a library asset as an explicit shot reference, then promote a keyframe.
	ref := p.ReferenceIDs[0]
	h.post("/projects/"+pid+"/refs", url.Values{"target": {"shot:" + sid}, "asset_id": {ref}, "op": {"add"}})
	p, _ = h.ps.Get(pid)
	if len(p.Shots[0].ReferenceIDs) != 1 {
		t.Errorf("ref not attached")
	}

	// Library pages and picker API.
	h.get("/assets")
	h.get("/assets?kind=keyframe&project=" + pid)
	var items []map[string]any
	if err := json.Unmarshal([]byte(h.get("/api/assets?project="+pid)), &items); err != nil || len(items) < 3 {
		t.Fatalf("api assets: %v %d", err, len(items))
	}
	if items[0]["id"] == nil || items[0]["url"] == nil {
		t.Errorf("api item fields: %v", items[0])
	}
	resp, _ = h.c.Get(h.srv.URL + "/assets/" + ref + "/file")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("asset file: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp.Body.Close()

	// Deleting an asset unlinks it everywhere.
	h.post("/assets/"+ref+"/delete", url.Values{})
	p, _ = h.ps.Get(pid)
	if len(p.ReferenceIDs) != 0 || len(p.Shots[0].ReferenceIDs) != 0 {
		t.Errorf("deleted asset still referenced: %+v", p)
	}
}

func TestSettingsAndPrompts(t *testing.T) {
	h := newHarness(t)
	page := h.get("/settings")
	if !strings.Contains(page, "ANTHROPIC_API_KEY") {
		t.Fatal("settings page missing providers")
	}
	form := url.Values{
		"p_orig":       {"anthropic", "fal", ""},
		"p_name":       {"claude", "fal", "local"},
		"p_type":       {"anthropic", "fal", "openai_chat"},
		"p_base_url":   {"", "", "http://localhost:11434/v1"},
		"p_key":        {"sk-secret", "", ""},
		"p_key_env":    {"", "FAL_KEY", ""},
		"p_headers":    {"", "", "X-Test: 1"},
		"img_provider": {"fal"},
		"img_model":    {"fal-ai/flux/dev"},
		"img_aspect":   {"9:16"},
		"img_extra":    {`{"num_inference_steps": 20}`},
	}
	for _, st := range config.LLMStages {
		form.Set("st_"+st+"_provider", "anthropic") // old name: must follow the rename
		form.Set("st_"+st+"_model", "m")
	}
	form.Set("st_idea_temperature", "0.7")
	h.post("/settings", form)
	page = h.get("/settings")
	if strings.Contains(page, "sk-secret") {
		t.Error("API key rendered in page")
	}
	if !strings.Contains(page, "•••• stored") || !strings.Contains(page, "localhost:11434") {
		t.Error("settings not saved")
	}

	h.get("/prompts")
	h.post("/prompts/idea.user", url.Values{"source": {"custom {{.P.Idea}}"}})
	if !strings.Contains(h.get("/prompts"), "customised") {
		t.Error("prompt override not shown")
	}
	h.post("/prompts/idea.user/reset", url.Values{})
	if strings.Contains(h.get("/prompts"), "customised") {
		t.Error("prompt override not reset")
	}
}

func TestCrossOriginRejected(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.srv.URL+"/projects", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: got %d", resp.StatusCode)
	}
	if list, _ := h.ps.List(); len(list) != 0 {
		t.Error("project created by cross-origin request")
	}
}

func TestBackRedirectValidation(t *testing.T) {
	for in, want := range map[string]string{
		"/projects/x#s1":       "/projects/x#s1",
		"//evil.example/":      "/def",
		"https://evil.example": "/def",
		"/\\evil.example":      "/def",
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader("back="+url.QueryEscape(in)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := back(r, "/def"); got != want {
			t.Errorf("back(%q) = %q, want %q", in, got, want)
		}
	}
	if got := withQuery("/p/x#s2", "err", "a b"); got != "/p/x?err=a+b#s2" {
		t.Errorf("withQuery: %s", got)
	}
}

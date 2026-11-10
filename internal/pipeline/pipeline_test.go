package pipeline

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

// fakeLLM answers based on which stage's system prompt it sees.
type fakeLLM struct {
	mu   sync.Mutex
	reqs []*llm.Request
}

func (f *fakeLLM) reply(req *llm.Request) string {
	switch {
	case strings.Contains(req.System, "senior screenwriter"):
		return "# Title\nThe Clockmaker's Daughter"
	case strings.Contains(req.System, "art director"):
		return "Here you go:\n```json\n" + `{"style":"cinematic, teal and orange","items":[
			{"kind":"character","name":"Lin","description":"heroine","prompt":"young woman, red coat"},
			{"kind":"location","name":"Clock Shop","description":"old shop","prompt":"dusty clock shop interior"}]}` + "\n```"
	case strings.Contains(req.System, "storyboard director"):
		return `{"shots":[
			{"scene":"Clock Shop, night","characters":["Lin"],"action":"Lin opens the box","dialogue":"","camera":"close-up","duration":"3s"},
			{"scene":"Street","characters":"Lin, Stranger","action":"She runs","camera":"wide","duration":5}]}`
	case strings.Contains(req.System, "cinematographer"):
		return `{"storyboard_prompt":"特写","keyframe_prompt":"close-up of a young woman","motion_prompt":"slow push in"}`
	}
	return "?"
}

func (f *fakeLLM) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return &llm.Response{Text: f.reply(req)}, nil
}

func (f *fakeLLM) Stream(ctx context.Context, req *llm.Request) iter.Seq2[llm.Chunk, error] {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	text := f.reply(req)
	return func(yield func(llm.Chunk, error) bool) {
		half := len(text) / 2
		if yield(llm.Chunk{Delta: text[:half]}, nil) {
			yield(llm.Chunk{Delta: text[half:], FinishReason: "stop"}, nil)
		}
	}
}

type fakeImage struct {
	mu   sync.Mutex
	reqs []*imagegen.Request
}

func (f *fakeImage) Generate(ctx context.Context, req *imagegen.Request) (*imagegen.Result, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return &imagegen.Result{Images: []imagegen.Image{{MIME: "image/png", Data: []byte("png:" + req.Prompt)}}}, nil
}

func newTestService(t *testing.T) (*Service, *fakeLLM, *fakeImage) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "test") // providers are faked
	t.Setenv("FAL_KEY", "test")
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	projects, _ := project.Open(filepath.Join(dir, "projects"))
	lib, _ := assets.Open(filepath.Join(dir, "assets"))
	s := New(cfg, projects, lib, &Prompts{})
	s.Log, _ = reqlog.Open(filepath.Join(dir, "logs"))
	fl, fi := &fakeLLM{}, &fakeImage{}
	s.NewLLM = func(*config.Provider) (llm.Client, error) { return fl, nil }
	s.NewImage = func(*config.Provider) (imagegen.Generator, error) { return fi, nil }
	return s, fl, fi
}

func TestFullPipeline(t *testing.T) {
	s, fl, fi := newTestService(t)
	ctx := context.Background()
	p, _ := s.Projects.Create("Demo", "a girl finds a time machine in her father's clock shop")
	ref, _ := s.Assets.Put(assets.Asset{Kind: assets.KindReference, ProjectID: p.ID}, []byte("\x89PNG\r\n\x1a\nref"))
	s.Projects.Update(p.ID, func(p *project.Project) error { p.ReferenceIDs = []string{ref.ID}; return nil })

	run := func(stage, target string) {
		t.Helper()
		var deltas strings.Builder
		err := s.Run(ctx, RunRequest{ProjectID: p.ID, Stage: stage, Target: target}, func(e Event) {
			if e.Type == "delta" {
				deltas.WriteString(e.Text)
			}
		})
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
	}

	run(project.StageIdea, "")
	got, _ := s.Projects.Get(p.ID)
	if !strings.Contains(got.Story, "Clockmaker") {
		t.Fatalf("story not saved: %q", got.Story)
	}
	if got.Runs[project.StageIdea].Status != "done" || got.Runs[project.StageIdea].Model == "" {
		t.Errorf("run not recorded: %+v", got.Runs[project.StageIdea])
	}

	run(project.StageVisualPrompts, "")
	got, _ = s.Projects.Get(p.ID)
	if len(got.Visuals) != 2 || got.Visuals[1].Kind != "scene" || got.Style == "" {
		t.Fatalf("visuals not parsed: %+v", got.Visuals)
	}
	// Visual prompts stage sends reference images by default.
	last := fl.reqs[len(fl.reqs)-1]
	if n := len(last.Messages[0].Content); n != 2 || last.Messages[0].Content[0].Type != llm.PartImage {
		t.Errorf("reference image not attached: %+v", last.Messages[0].Content)
	}

	run(project.StageVisualImages, "all")
	got, _ = s.Projects.Get(p.ID)
	if len(got.Visuals[0].AssetIDs) != 1 || len(got.Visuals[1].AssetIDs) != 1 {
		t.Fatalf("concept images not linked: %+v", got.Visuals)
	}
	if !strings.HasSuffix(fi.reqs[0].Prompt, "cinematic, teal and orange") || fi.reqs[0].Model != "fal-ai/flux/dev" {
		t.Errorf("style/model not applied: %+v", fi.reqs[0])
	}
	if fi.reqs[0].AspectRatio != "9:16" {
		t.Errorf("aspect ratio not applied: %q", fi.reqs[0].AspectRatio)
	}

	run(project.StageStoryboard, "")
	got, _ = s.Projects.Get(p.ID)
	if len(got.Shots) != 2 || got.Shots[0].Duration != 3 || len(got.Shots[1].Characters) != 2 || got.Shots[1].Number != 2 {
		t.Fatalf("shots not parsed: %+v", got.Shots)
	}

	run(project.StageShotPrompts, "all")
	got, _ = s.Projects.Get(p.ID)
	for _, sh := range got.Shots {
		if sh.KeyframePrompt == "" || sh.MotionPrompt == "" || sh.StoryboardPrompt != "特写" {
			t.Fatalf("shot prompts not saved: %+v", sh)
		}
	}

	refs, auto := ShotRefs(got, &got.Shots[0])
	if !auto || len(refs) != 2 { // Lin + Clock Shop
		t.Errorf("auto refs: %v", refs)
	}

	n := len(fi.reqs)
	run(project.StageKeyframes, got.Shots[0].ID)
	got, _ = s.Projects.Get(p.ID)
	if len(got.Shots[0].KeyframeIDs) != 1 || len(got.Shots[1].KeyframeIDs) != 0 {
		t.Fatalf("keyframe not linked: %+v", got.Shots)
	}
	kreq := fi.reqs[n]
	if len(kreq.References) != 2 || kreq.Model != "fal-ai/flux-pro/kontext" {
		t.Errorf("keyframe request should use refs + ref model: %d %s", len(kreq.References), kreq.Model)
	}
	kf, _ := s.Assets.Get(got.Shots[0].KeyframeIDs[0])
	if kf.Kind != assets.KindKeyframe || kf.ShotID != got.Shots[0].ID || len(kf.Parents) != 2 || kf.ProjectID != p.ID {
		t.Errorf("keyframe asset metadata: %+v", kf)
	}

	// "only missing" skips the shot that already has a keyframe.
	n = len(fi.reqs)
	if err := s.Run(ctx, RunRequest{ProjectID: p.ID, Stage: project.StageKeyframes, Target: "all", OnlyMissing: true}, nil); err != nil {
		t.Fatal(err)
	}
	if len(fi.reqs)-n != 1 {
		t.Errorf("only-missing generated %d images, want 1", len(fi.reqs)-n)
	}
}

func TestRawOutputKeptOnParseFailure(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := s.Projects.Create("x", "y")
	err := s.ApplyStoryboardJSON(p.ID, "sorry, I cannot produce JSON")
	if err == nil {
		t.Fatal("expected parse error")
	}
	got, _ := s.Projects.Get(p.ID)
	if got.StoryboardRaw == "" {
		t.Error("raw output not saved")
	}
	if err := s.ApplyStoryboardJSON(p.ID, `{"shots":[{"scene":"a","action":"b"}]}`); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Projects.Get(p.ID)
	if got.StoryboardRaw != "" || len(got.Shots) != 1 {
		t.Errorf("apply did not clear raw / set shots: %+v", got)
	}
}

func TestPreconditionsAndBusy(t *testing.T) {
	s, _, _ := newTestService(t)
	p, _ := s.Projects.Create("x", "")
	err := s.Run(context.Background(), RunRequest{ProjectID: p.ID, Stage: project.StageIdea}, nil)
	if err == nil || !strings.Contains(err.Error(), "idea") {
		t.Errorf("want missing-idea error, got %v", err)
	}
	got, _ := s.Projects.Get(p.ID)
	if got.Runs[project.StageIdea].Status != "error" {
		t.Errorf("error not recorded: %+v", got.Runs)
	}
	s.acquire(p.ID+"/"+project.StageIdea, func() {})
	if err := s.Run(context.Background(), RunRequest{ProjectID: p.ID, Stage: project.StageIdea}, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("want ErrBusy, got %v", err)
	}
}

func TestExtractJSON(t *testing.T) {
	var v struct{ A int }
	for _, in := range []string{`{"A":1}`, "```json\n{\"A\":1}\n```", "Sure! {\"A\":1} Hope it helps."} {
		v.A = 0
		if err := extractJSON(in, &v); err != nil || v.A != 1 {
			t.Errorf("%q: %v %v", in, v, err)
		}
	}
}

func TestPromptOverride(t *testing.T) {
	dir := t.TempDir()
	pr := &Prompts{OverrideDir: dir}
	out, err := pr.Render("idea.user", promptData{P: &project.Project{Idea: "X"}})
	if err != nil || !strings.Contains(out, "X") {
		t.Fatalf("%q %v", out, err)
	}
	if err := assets.WriteFileAtomic(filepath.Join(dir, "idea.user.tmpl"), []byte("custom {{.P.Idea}}")); err != nil {
		t.Fatal(err)
	}
	out, _ = pr.Render("idea.user", promptData{P: &project.Project{Idea: "X"}})
	if out != "custom X" {
		t.Errorf("override not used: %q", out)
	}
}

func TestMissingKey(t *testing.T) {
	s, _, _ := newTestService(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	p, _ := s.Projects.Create("x", "idea")
	err := s.Run(context.Background(), RunRequest{ProjectID: p.ID, Stage: project.StageIdea}, nil)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("want missing key error, got %v", err)
	}
}

func TestRequestsAreLogged(t *testing.T) {
	s, _, _ := newTestService(t)
	ctx := context.Background()
	p, _ := s.Projects.Create("Demo", "an idea")
	ref, _ := s.Assets.Put(assets.Asset{Kind: assets.KindReference, ProjectID: p.ID, MIME: "image/png"}, []byte("\x89PNG\r\n\x1a\nref"))
	s.Projects.Update(p.ID, func(p *project.Project) error { p.ReferenceIDs = []string{ref.ID}; return nil })
	for _, st := range []string{project.StageIdea, project.StageVisualPrompts, project.StageVisualImages} {
		if err := s.Run(ctx, RunRequest{ProjectID: p.ID, Stage: st}, nil); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	days, _ := s.Log.Days()
	if len(days) != 1 || days[0].Count != 4 { // idea, visual prompts, 2 concept images
		t.Fatalf("days: %+v", days)
	}
	list, _ := s.Log.List(days[0].Date)
	byStage := map[string]*reqlog.Entry{}
	for _, e := range list {
		byStage[e.Stage] = e
		if e.Status != "ok" || e.ProjectID != p.ID || e.Provider == "" || e.Model == "" {
			t.Errorf("incomplete entry: %+v", e)
		}
	}
	idea := byStage[project.StageIdea]
	if idea.Kind != reqlog.KindLLM || !strings.Contains(idea.Request["user"].(string), "an idea") ||
		!strings.Contains(idea.Request["system"].(string), "screenwriter") || !strings.Contains(idea.Response["text"].(string), "Clockmaker") ||
		idea.Response["finish_reason"] != "stop" {
		t.Errorf("idea entry: %+v", idea)
	}
	// Reference images are logged by asset ID (visual prompts send them by default).
	if imgs, _ := byStage[project.StageVisualPrompts].Request["images"].([]any); len(imgs) != 1 || imgs[0] != ref.ID {
		t.Errorf("images: %v", byStage[project.StageVisualPrompts].Request["images"])
	}
	img := byStage[project.StageVisualImages]
	if img.Kind != reqlog.KindImage || img.Target == "" || img.Label == "" || !strings.Contains(img.Request["prompt"].(string), "teal and orange") {
		t.Errorf("image entry: %+v", img)
	}
	if out, _ := img.Response["assets"].([]any); len(out) != 1 {
		t.Errorf("image outputs: %v", img.Response["assets"])
	}

	// Failures are logged with the error.
	s.NewLLM = func(*config.Provider) (llm.Client, error) { return failingLLM{}, nil }
	if err := s.Run(ctx, RunRequest{ProjectID: p.ID, Stage: project.StageIdea}, nil); err == nil {
		t.Fatal("expected failure")
	}
	list, _ = s.Log.List(days[0].Date)
	if len(list) != 5 || list[0].Status != "error" || !strings.Contains(list[0].Error, "upstream down") {
		t.Errorf("failure not logged: %+v", list[0])
	}
}

type failingLLM struct{}

func (failingLLM) Chat(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, errors.New("upstream down")
}

func (failingLLM) Stream(context.Context, *llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		if yield(llm.Chunk{Delta: "partial"}, nil) {
			yield(llm.Chunk{}, errors.New("upstream down"))
		}
	}
}

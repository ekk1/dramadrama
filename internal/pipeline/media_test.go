package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/catalog"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

// fakeQueue is a queue-based generator: jobs are submitted and collected
// separately, and Wait can be made to hang until the run is stopped.
type fakeQueue struct {
	mu      sync.Mutex
	hang    bool
	fail    error
	submits []*imagegen.Request
	waited  []string
	cancels []string
	waiting chan struct{} // receives when a Wait starts hanging
}

func (f *fakeQueue) Generate(ctx context.Context, req *imagegen.Request) (*imagegen.Result, error) {
	job, err := f.Submit(ctx, req)
	if err != nil {
		return nil, err
	}
	return f.Wait(ctx, job, nil)
}

func (f *fakeQueue) Submit(ctx context.Context, req *imagegen.Request) (*imagegen.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, req)
	id := fmt.Sprintf("job%d-%s", len(f.submits), req.Kind)
	return &imagegen.Job{RequestID: id, StatusURL: "s/" + id + "/status", ResponseURL: "s/" + id}, nil
}

func (f *fakeQueue) Wait(ctx context.Context, job *imagegen.Job, onStatus func(string)) (*imagegen.Result, error) {
	f.mu.Lock()
	hang, fail := f.hang, f.fail
	f.waited = append(f.waited, job.RequestID)
	f.mu.Unlock()
	if onStatus != nil {
		onStatus("IN_PROGRESS")
	}
	if hang {
		if f.waiting != nil {
			f.waiting <- struct{}{}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fail != nil {
		return nil, fail
	}
	res := &imagegen.Result{RequestID: job.RequestID}
	if strings.HasSuffix(job.RequestID, imagegen.KindVideo) {
		res.Videos = []imagegen.Image{{MIME: "video/mp4", Data: []byte("mp4:" + job.RequestID)}}
	} else {
		res.Images = []imagegen.Image{{MIME: "image/png", Data: []byte("png:" + job.RequestID)}}
	}
	return res, nil
}

func (f *fakeQueue) Cancel(ctx context.Context, job *imagegen.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, job.RequestID)
	return nil
}

func (f *fakeQueue) DescribePayload(req *imagegen.Request) map[string]any {
	return map[string]any{"prompt": req.Prompt, "last": req.LastFrame != nil, "videos": len(req.RefVideos)}
}

func (f *fakeQueue) Explain(req *imagegen.Request) []string {
	if req.Kind != imagegen.KindVideo {
		return nil
	}
	return []string{"first frame → image_url"}
}

// clipFixture is a project with one shot that has every video input.
func clipFixture(t *testing.T) (*Service, *fakeQueue, *project.Project, string) {
	t.Helper()
	s, _, _ := newTestService(t)
	q := &fakeQueue{waiting: make(chan struct{}, 4)}
	s.NewImage = func(*config.Provider) (imagegen.Generator, error) { return q, nil }
	s.Models, _ = catalog.Open(t.TempDir())
	p, _ := s.Projects.Create("Clips", "idea")
	put := func(mime, data string) string {
		a, err := s.Assets.Put(assets.Asset{ProjectID: p.ID, MIME: mime}, []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	shot := project.Shot{
		ID: "s_1", Number: 1, Scene: "Shop", Duration: 6, MotionPrompt: "she turns to the door",
		KeyframeIDs: []string{put("image/png", "old-kf"), put("image/png", "kf")},
		EndFrameIDs: []string{put("image/png", "end")},
		VideoRefIDs: []string{put("video/mp4", "motion")},
		AudioRefIDs: []string{put("audio/mpeg", "voice"), "a_missing"},
	}
	p, _ = s.Projects.Update(p.ID, func(p *project.Project) error {
		p.Shots = []project.Shot{shot, {ID: "s_2", Number: 2, MotionPrompt: ""}}
		return nil
	})
	return s, q, p, "s_1"
}

func TestClips(t *testing.T) {
	s, q, p, sid := clipFixture(t)
	fetches := 0
	s.FetchSchema = func(_ context.Context, prov *config.Provider, model string) (*imagegen.InputSchema, error) {
		fetches++
		if prov.Name != "fal" || model != config.DefaultVideoModel {
			t.Errorf("schema asked for %s / %s", prov.Name, model)
		}
		return &imagegen.InputSchema{Model: model, Params: []imagegen.Param{{Name: "prompt", Type: "string"}, {Name: "image_url", Type: "string"}}}, nil
	}
	var status []string
	run := func(target string) error {
		return s.Run(context.Background(), RunRequest{ProjectID: p.ID, Stage: project.StageClips, Target: target}, func(e Event) {
			if e.Type == "status" {
				status = append(status, e.Text)
			}
		})
	}
	if err := run(sid); err != nil {
		t.Fatal(err)
	}
	req := q.submits[0]
	if req.Kind != imagegen.KindVideo || req.Model != config.DefaultVideoModel || req.Prompt != "she turns to the door" ||
		req.Duration != 6 || req.AspectRatio != "9:16" || req.Schema == nil {
		t.Errorf("request: %+v", req)
	}
	// The keyframe in use (the last one) opens the clip; the end frame closes it.
	if req.FirstFrame == nil || string(req.FirstFrame.Data) != "kf" || req.LastFrame == nil || string(req.LastFrame.Data) != "end" {
		t.Errorf("frames: %+v %+v", req.FirstFrame, req.LastFrame)
	}
	if len(req.RefVideos) != 1 || len(req.RefAudios) != 1 || req.RefAudios[0].MIME != "audio/mpeg" {
		t.Errorf("reference media: %d videos, %d audios", len(req.RefVideos), len(req.RefAudios))
	}
	got, _ := s.Projects.Get(p.ID)
	sh := got.Shot(sid)
	if len(sh.ClipIDs) != 1 || len(got.Jobs) != 0 {
		t.Fatalf("clips %v, jobs %v", sh.ClipIDs, got.Jobs)
	}
	clip, data, _ := s.Assets.Read(sh.Clip())
	if clip.Kind != assets.KindVideo || clip.Media() != "video" || clip.ShotID != sid || clip.Stage != project.StageClips ||
		len(clip.Parents) != 4 || !strings.HasPrefix(string(data), "mp4:") {
		t.Errorf("clip asset: %+v", clip)
	}
	if r := got.Runs[project.RunKey(project.StageClips, sid)]; r == nil || r.Status != "done" || r.Model != config.DefaultVideoModel {
		t.Errorf("run: %+v", r)
	}
	joined := strings.Join(status, "\n")
	for _, want := range []string{"Fetching the parameter list", "first frame → image_url", "Generating video with fal", "Provider status: IN_PROGRESS", "Saved "} {
		if !strings.Contains(joined, want) {
			t.Errorf("status lines missing %q:\n%s", want, joined)
		}
	}
	// The request is logged with its inputs by asset ID.
	days, _ := s.Log.Days()
	list, _ := s.Log.List(days[0].Date)
	if len(list) != 1 || list[0].Kind != reqlog.KindVideo || list[0].Request["first_frame"] != sh.Keyframe() || list[0].Request["last_frame"] != sh.EndFrame() {
		t.Errorf("log: %+v", list)
	}

	// The schema is cached: generating again does not fetch it again.
	if err := run(sid); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Errorf("schema fetched %d times", fetches)
	}
	// "All" with only-missing skips the shot that has a clip; the other
	// one has no motion prompt and fails with a clear message.
	err := s.Run(context.Background(), RunRequest{ProjectID: p.ID, Stage: project.StageClips, Target: "all", OnlyMissing: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "no motion prompt") || len(q.submits) != 2 {
		t.Errorf("only missing: %v, %d submits", err, len(q.submits))
	}
}

func TestClipWithoutSchemaOrKeyframe(t *testing.T) {
	s, q, p, _ := clipFixture(t)
	s.FetchSchema = func(context.Context, *config.Provider, string) (*imagegen.InputSchema, error) {
		return nil, errors.New("catalogue unreachable")
	}
	s.Projects.Update(p.ID, func(p *project.Project) error { p.Shots[1].MotionPrompt = "rain falls"; return nil })
	var status []string
	err := s.Run(context.Background(), RunRequest{ProjectID: p.ID, Stage: project.StageClips, Target: "s_2", Model: "other/model"}, func(e Event) {
		status = append(status, e.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	if req := q.submits[0]; req.Schema != nil || req.FirstFrame != nil || req.Model != "other/model" {
		t.Errorf("request: %+v", req)
	}
	joined := strings.Join(status, "\n")
	if !strings.Contains(joined, "catalogue unreachable") || !strings.Contains(joined, "no keyframe") {
		t.Errorf("status:\n%s", joined)
	}
}

func TestJobIsResumed(t *testing.T) {
	s, q, p, sid := clipFixture(t)
	s.FetchSchema = nil
	key := project.RunKey(project.StageClips, sid)
	req := RunRequest{ProjectID: p.ID, Stage: project.StageClips, Target: sid}

	// Start a clip and stop it while the provider is still working.
	stopRun := func() error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- s.Run(context.Background(), req, nil) }()
		select {
		case <-q.waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("run never reached the provider")
		}
		if !s.Cancel(p.ID, key) {
			t.Fatal("nothing to cancel")
		}
		return <-done
	}
	q.hang = true
	if err := stopRun(); err == nil || err.Error() != "stopped" {
		t.Fatalf("stopped run returned %v", err)
	}
	got, _ := s.Projects.Get(p.ID)
	job := got.Jobs[key]
	if job == nil || job.RequestID != "job1-video" || job.Provider != "fal" || job.Fingerprint == "" || len(got.Shot(sid).ClipIDs) != 0 {
		t.Fatalf("job not kept: %+v", got.Jobs)
	}

	// Running again collects that job: nothing new is submitted.
	q.hang = false
	var status []string
	if err := s.Run(context.Background(), req, func(e Event) { status = append(status, e.Text) }); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Projects.Get(p.ID)
	if len(q.submits) != 1 || q.waited[len(q.waited)-1] != "job1-video" || len(got.Shot(sid).ClipIDs) != 1 || len(got.Jobs) != 0 {
		t.Errorf("not resumed: %d submits, waited %v, jobs %v", len(q.submits), q.waited, got.Jobs)
	}
	if !strings.Contains(strings.Join(status, "\n"), "Collecting the result of request job1-video") {
		t.Errorf("status: %v", status)
	}

	// If the request changed in the meantime, the old job is cancelled and
	// a new one submitted.
	q.hang = true
	stopRun()
	s.Projects.Update(p.ID, func(p *project.Project) error { p.Shot(sid).MotionPrompt = "she runs out"; return nil })
	q.hang = false
	if err := s.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if len(q.submits) != 3 || len(q.cancels) != 1 || q.cancels[0] != "job2-video" || q.waited[len(q.waited)-1] != "job3-video" {
		t.Errorf("changed request: %d submits, cancels %v, waited %v", len(q.submits), q.cancels, q.waited)
	}

	// A job the provider reports as failed is final: it is dropped.
	q.fail = errors.New("fal: unexpected status \"FAILED\"")
	if err := s.Run(context.Background(), req, nil); err == nil {
		t.Fatal("expected failure")
	}
	if got, _ := s.Projects.Get(p.ID); len(got.Jobs) != 0 {
		t.Errorf("failed job kept: %v", got.Jobs)
	}

	// An unfinished job can be discarded; the provider is told.
	q.fail, q.hang = nil, true
	stopRun()
	if !s.DiscardJob(context.Background(), p.ID, key) || s.DiscardJob(context.Background(), p.ID, key) {
		t.Error("discard")
	}
	if got, _ := s.Projects.Get(p.ID); len(got.Jobs) != 0 || q.cancels[len(q.cancels)-1] != "job5-video" {
		t.Errorf("after discard: jobs %v, cancels %v", got.Jobs, q.cancels)
	}
}

func TestImageJobIsResumed(t *testing.T) {
	s, q, p, sid := clipFixture(t)
	s.Projects.Update(p.ID, func(p *project.Project) error { p.Shot(sid).KeyframePrompt = "a shop at night"; return nil })
	req := RunRequest{ProjectID: p.ID, Stage: project.StageKeyframes, Target: sid}
	key := project.RunKey(project.StageKeyframes, sid)
	q.hang = true
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), req, nil) }()
	<-q.waiting
	s.Cancel(p.ID, key)
	<-done
	q.hang = false
	if err := s.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Projects.Get(p.ID)
	if len(q.submits) != 1 || len(got.Shot(sid).KeyframeIDs) != 3 || len(got.Jobs) != 0 {
		t.Errorf("image job not resumed: %d submits, %d keyframes, jobs %v", len(q.submits), len(got.Shot(sid).KeyframeIDs), got.Jobs)
	}
}

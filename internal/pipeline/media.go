package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

// This file holds what image and video generation share: resumable
// provider jobs, loading input files, and storing and logging outputs.

// mediaResult is what one media request produced.
type mediaResult struct {
	Assets   []*assets.Asset
	Provider string
	Model    string
}

func assetIDs(list []*assets.Asset) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}

// forEachMedia is forEach for media stages: fn generates for one item, and
// the provider and model of the last request are returned for the run
// record.
func forEachMedia(ctx context.Context, ids, labels []string, emit Emit, fn func(id string) (*mediaResult, error)) (string, string, error) {
	var last *mediaResult
	err := forEach(ctx, ids, labels, emit, func(id string) error {
		res, err := fn(id)
		if res != nil {
			last = res
		}
		return err
	})
	if last == nil {
		return "", "", err
	}
	return last.Provider, last.Model, err
}

// saveOutputs stores generated files as assets described by meta, adds
// them to res, and records where they came from.
func (s *Service) saveOutputs(files []imagegen.Image, meta assets.Asset, out *imagegen.Result, res *mediaResult, emit Emit) error {
	for _, f := range files {
		m := meta
		m.MIME, m.Width, m.Height = f.MIME, f.Width, f.Height
		m.Params = maps.Clone(meta.Params)
		if out.Seed != nil {
			m.Params["seed"] = *out.Seed
		}
		if out.RequestID != "" {
			m.Params["request_id"] = out.RequestID
		}
		a, err := s.Assets.Put(m, f.Data)
		if err != nil {
			return err
		}
		emit(Event{Type: "status", Text: "Saved " + a.ID})
		res.Assets = append(res.Assets, a)
	}
	return nil
}

// logOutputs writes the log entry of a media request that the provider
// answered, once its outputs are stored (or failed to be).
func (s *Service) logOutputs(entry *reqlog.Entry, start time.Time, out *imagegen.Result, res *mediaResult, err error, emit Emit) {
	entry.Response["assets"] = assetIDs(res.Assets)
	if out.Seed != nil {
		entry.Response["seed"] = *out.Seed
	}
	if out.RequestID != "" {
		entry.Response["request_id"] = out.RequestID
	}
	if len(out.Raw) > 0 {
		entry.Response["raw"] = out.Raw
	}
	s.writeLog(entry, start, err, emit)
}

// ---------------------------------------------------------------------------
// Resumable provider jobs

// fingerprint identifies what a request asks for, so that a job is only
// resumed by a run that would have sent the same request.
func fingerprint(gen imagegen.Generator, req *imagegen.Request) string {
	var body any = []any{req.Prompt, req.AspectRatio, req.Duration}
	if d, ok := gen.(imagegen.PayloadDescriber); ok {
		body = d.DescribePayload(req) // inline files appear as type + size
	}
	b, _ := json.Marshal([]any{req.Kind, req.Model, body})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (s *Service) setJob(projectID, key string, job *project.Job) {
	_, _ = s.Projects.Update(projectID, func(p *project.Project) error {
		if job == nil {
			delete(p.Jobs, key)
			return nil
		}
		if p.Jobs == nil {
			p.Jobs = map[string]*project.Job{}
		}
		p.Jobs[key] = job
		return nil
	})
}

// runJob sends one media request and returns its result.
//
// With a queue-based provider the accepted job is saved in the project
// (Project.Jobs[key]) until its result has been collected. If a run is
// stopped or the server restarts meanwhile, the next run for the same
// item with an unchanged request collects that job's result instead of
// submitting, and paying for, a new one. A job whose request has since
// changed is cancelled and replaced.
func (s *Service) runJob(ctx context.Context, projectID, key, provName string, gen imagegen.Generator, req *imagegen.Request, emit Emit) (*imagegen.Result, error) {
	q, ok := gen.(imagegen.Queued)
	if !ok {
		return gen.Generate(ctx, req)
	}
	fp := fingerprint(gen, req)
	var job *imagegen.Job
	if p, err := s.Projects.Get(projectID); err == nil {
		if pj := p.Jobs[key]; pj != nil {
			old := &imagegen.Job{RequestID: pj.RequestID, StatusURL: pj.StatusURL, ResponseURL: pj.ResponseURL}
			if pj.Fingerprint == fp && pj.Provider == provName {
				emit(Event{Type: "status", Text: fmt.Sprintf("Collecting the result of request %s, submitted earlier, instead of generating again…", pj.RequestID)})
				job = old
			} else {
				emit(Event{Type: "status", Text: "The request changed since the unfinished one was submitted: cancelling that one."})
				_ = q.Cancel(ctx, old)
				s.setJob(projectID, key, nil)
			}
		}
	}
	if job == nil {
		var err error
		if job, err = q.Submit(ctx, req); err != nil {
			return nil, err
		}
		s.setJob(projectID, key, &project.Job{
			Provider: provName, Model: req.Model, RequestID: job.RequestID, StatusURL: job.StatusURL, ResponseURL: job.ResponseURL,
			Fingerprint: fp, SubmittedAt: time.Now().UTC(),
		})
	}
	res, err := q.Wait(ctx, job, func(state string) {
		emit(Event{Type: "status", Text: "Provider status: " + state})
	})
	if imagegen.Retryable(err) {
		// The job may still finish: keep it so it can be collected later.
		emit(Event{Type: "status", Text: "The request is still with the provider. Run this step again to collect its result."})
		return nil, err
	}
	s.setJob(projectID, key, nil)
	return res, err
}

// DiscardJob forgets an unfinished provider job and asks the provider to
// cancel it (best effort). It reports whether there was one.
func (s *Service) DiscardJob(ctx context.Context, projectID, key string) bool {
	p, err := s.Projects.Get(projectID)
	if err != nil || p.Jobs[key] == nil {
		return false
	}
	pj := p.Jobs[key]
	s.setJob(projectID, key, nil)
	if prov := s.Config.Get().Provider(pj.Provider); prov != nil {
		if gen, err := s.NewImage(prov); err == nil {
			if q, ok := gen.(imagegen.Queued); ok {
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				_ = q.Cancel(ctx, &imagegen.Job{RequestID: pj.RequestID, StatusURL: pj.StatusURL, ResponseURL: pj.ResponseURL})
			}
		}
	}
	return true
}

// loadMedia reads assets of one media type as request inputs, skipping
// missing ones. It returns the inputs and the IDs actually used.
func (s *Service) loadMedia(ids []string, media string) (out []imagegen.Image, used []string) {
	for _, id := range ids {
		a, data, err := s.Assets.Read(id)
		if err != nil || a.Media() != media {
			continue
		}
		out = append(out, imagegen.Image{MIME: a.MIME, Data: data})
		used = append(used, id)
	}
	return out, used
}

package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ekk1/dramadrama/internal/pipeline"
	"github.com/ekk1/dramadrama/internal/project"
)

// ---------------------------------------------------------------------------
// Running steps (Server-Sent Events)

// handleRun executes one step and streams progress as SSE. The step runs
// detached from the request: closing the page does not stop it (use
// /cancel), and the result is saved either way.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.projects.Get(id); notFound(w, err) {
		return
	}
	req := pipeline.RunRequest{
		ProjectID:   id,
		Stage:       r.PathValue("stage"),
		Target:      r.FormValue("target"),
		Provider:    strings.TrimSpace(r.FormValue("provider")),
		Model:       strings.TrimSpace(r.FormValue("model")),
		OnlyMissing: r.FormValue("only_missing") != "",
	}
	key := project.RunKey(req.Stage, req.Target)
	fk := id + "/" + key
	// If this step is already running, the request follows that run.
	feed, created := s.feeds.open(fk)
	if created {
		ctx := context.WithoutCancel(r.Context()) // the run outlives the request
		go func() {
			err := s.pipe.Run(ctx, req, func(e pipeline.Event) { feed.publish(e.Type, e.Text, false) })
			s.feeds.remove(fk)
			if err != nil {
				log.Printf("run %s %s %s: %v", id, req.Stage, req.Target, err)
				feed.publish("error", err.Error(), true)
				return
			}
			feed.publish("done", "ok", true)
		}()
	}
	s.follow(w, r, key, feed)
}

// handleEvents follows a run that is in progress, from its first event.
// A page uses it to pick up the runs it finds running when it loads.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	feed := s.feeds.get(r.PathValue("id") + "/" + key)
	if feed == nil {
		http.Error(w, "that step is not running", http.StatusNotFound)
		return
	}
	s.follow(w, r, key, feed)
}

// handleDiscardJob forgets an unfinished provider job, so the next run
// submits a new request instead of collecting that one.
func (s *Server) handleDiscardJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.pipe.DiscardJob(r.Context(), id, r.FormValue("key"))
	done(w, r, projectURL(id), nil)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.pipe.Cancel(id, r.FormValue("key")) {
		done(w, r, projectURL(id), errors.New("that step is not running"))
		return
	}
	done(w, r, projectURL(id), nil)
}

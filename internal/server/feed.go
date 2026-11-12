package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// A runFeed holds the events of one run that was started over HTTP, so
// that any number of pages can follow it: the one that started it, and
// pages opened or reloaded while it is still going.
type runFeed struct {
	mu     sync.Mutex
	events []feedEvent
	done   bool
	wake   chan struct{} // closed, and replaced, on every change
}

type feedEvent struct{ Type, Text string }

func (f *runFeed) publish(typ, text string, last bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, feedEvent{typ, text})
	f.done = f.done || last
	close(f.wake)
	f.wake = make(chan struct{})
}

// since returns the events from index i on, whether the run has ended,
// and a channel that is closed when there is more.
func (f *runFeed) since(i int) ([]feedEvent, bool, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[i:], f.done, f.wake
}

// feeds are the runs in progress, by project ID + "/" + run key.
type feeds struct {
	mu sync.Mutex
	m  map[string]*runFeed
}

// open returns the feed of the run in progress for k, or a new one with
// created set.
func (fs *feeds) open(k string) (f *runFeed, created bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if f := fs.m[k]; f != nil {
		return f, false
	}
	if fs.m == nil {
		fs.m = map[string]*runFeed{}
	}
	f = &runFeed{wake: make(chan struct{})}
	fs.m[k] = f
	return f, true
}

func (fs *feeds) get(k string) *runFeed {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.m[k]
}

func (fs *feeds) remove(k string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.m, k)
}

// follow streams a feed as Server-Sent Events, from its first event, until
// the run ends or the client goes away.
func (s *Server) follow(w http.ResponseWriter, r *http.Request, key string, f *runFeed) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event, data string) bool {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send("key", key) {
		return
	}
	tr := translator(langOf(r))
	keepAlive := time.NewTicker(15 * time.Second) // while waiting on slow providers
	defer keepAlive.Stop()
	for i := 0; ; {
		events, done, wake := f.since(i)
		for _, e := range events {
			if e.Type == "status" || e.Type == "error" {
				e.Text = tr(e.Text)
			}
			if !send(e.Type, e.Text) {
				return
			}
		}
		i += len(events)
		if done {
			return
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if !send("ping", "") {
				return
			}
		}
	}
}

package project

import (
	"errors"
	"testing"
)

func TestStore(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Create("  ", "a girl finds a time machine")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Untitled" || p.AspectRatio != "9:16" {
		t.Errorf("defaults not applied: %+v", p)
	}
	_, err = s.Update(p.ID, func(p *Project) error {
		p.Shots = []Shot{{ID: "s1"}, {ID: "s2", KeyframeIDs: []string{"a1", "a2"}}}
		p.ReferenceIDs = []string{"a1"}
		p.RemoveAssetRefs("a1")
		p.Renumber()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Shot("s2").Number != 2 || len(got.Shot("s2").KeyframeIDs) != 1 || len(got.ReferenceIDs) != 0 {
		t.Errorf("update not persisted: %+v", got)
	}
	// Failed update does not save.
	boom := errors.New("boom")
	if _, err := s.Update(p.ID, func(p *Project) error { p.Name = "x"; return boom }); err != boom {
		t.Fatal(err)
	}
	if got, _ := s.Get(p.ID); got.Name != "Untitled" {
		t.Error("failed update was saved")
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Errorf("list: %d", len(list))
	}
	if _, err := s.Get("../etc"); err != ErrNotFound {
		t.Errorf("path traversal not rejected: %v", err)
	}
	if err := s.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(p.ID); err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

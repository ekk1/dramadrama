package assets

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	img := pngBytes(t, 9, 16)
	a, err := s.Put(Asset{Kind: KindConcept, ProjectID: "p1", Name: "Hero", Tags: []string{"Hero", " main "}}, img)
	if err != nil {
		t.Fatal(err)
	}
	if a.MIME != "image/png" || a.Ext != ".png" || a.Width != 9 || a.Height != 16 {
		t.Errorf("metadata not detected: %+v", a)
	}
	if len(a.Tags) != 2 || a.Tags[0] != "hero" || a.Tags[1] != "main" {
		t.Errorf("tags not normalised: %v", a.Tags)
	}
	// Same bytes again: new record, shared file.
	b, err := s.Put(Asset{Kind: KindReference}, img)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.SHA256 != b.SHA256 {
		t.Fatal("expected distinct records over the same content")
	}

	// Reload from disk.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.List(Filter{ProjectID: "p1"}); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("project filter: %v", got)
	}
	if got := s2.List(Filter{Kind: KindReference}); len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("kind filter: %v", got)
	}
	if got := s2.List(Filter{Query: "her"}); len(got) != 1 {
		t.Errorf("query filter: %v", got)
	}
	if got := s2.List(Filter{Tag: "main"}); len(got) != 1 {
		t.Errorf("tag filter: %v", got)
	}

	if _, err := s2.Update(a.ID, func(x *Asset) { x.Name = "Heroine"; x.SHA256 = "nope" }); err != nil {
		t.Fatal(err)
	}
	got, _ := s2.Get(a.ID)
	if got.Name != "Heroine" || got.SHA256 != a.SHA256 {
		t.Errorf("update: %+v", got)
	}

	// Deleting one record keeps the shared file; deleting both removes it.
	if err := s2.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s2.FilePath(b)); err != nil {
		t.Errorf("shared file removed too early: %v", err)
	}
	if err := s2.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s2.FilePath(b)); !os.IsNotExist(err) {
		t.Errorf("file not removed: %v", err)
	}
	if _, err := s2.Get(a.ID); err != ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestParseTags(t *testing.T) {
	got := ParseTags("a, B  c,,a")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("got %v", got)
	}
}

package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ekk1/dramadrama/internal/config"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get("fal") != nil {
		t.Fatal("unexpected list")
	}
	// Provider names are escaped, so they cannot leave the directory.
	name := "../evil/name"
	if _, err := s.Save(name, []Model{{ID: "m1", Category: "text-to-image"}}); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].IsDir() {
		t.Fatalf("files: %v", files)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil")); err == nil {
		t.Fatal("escaped the store directory")
	}
	l := s.Get(name)
	if l == nil || len(l.Models) != 1 || l.Models[0].ID != "m1" || l.FetchedAt.IsZero() {
		t.Fatalf("get: %+v", l)
	}
	s.Rename(name, "fal")
	if s.Get(name) != nil || s.Get("fal") == nil {
		t.Fatal("rename")
	}
	s.Delete("fal")
	if s.Get("fal") != nil {
		t.Fatal("delete")
	}
	var none *Store
	none.Delete("x")
	if none.Get("x") != nil {
		t.Fatal("nil store")
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("x-api-key") != "k" {
				http.Error(w, "no key", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}]}`))
		case "/cat/models":
			w.Write([]byte(`{"models":[{"endpoint_id":"fal-ai/flux/dev","metadata":{"display_name":"FLUX","category":"text-to-image"}}],"has_more":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := Fetch(context.Background(), &config.Provider{Name: "a", Type: "anthropic", BaseURL: srv.URL, APIKey: "k"})
	if err != nil || len(got) != 1 || got[0].Name != "Claude Opus 5.5" {
		t.Fatalf("llm: %+v %v", got, err)
	}
	got, err = Fetch(context.Background(), &config.Provider{Name: "f", Type: "fal", CatalogURL: srv.URL + "/cat"})
	if err != nil || len(got) != 1 || got[0].Category != "text-to-image" {
		t.Fatalf("fal: %+v %v", got, err)
	}
	if _, err := Fetch(context.Background(), &config.Provider{Name: "x", Type: "nope"}); err == nil {
		t.Fatal("unknown type should fail")
	}
}

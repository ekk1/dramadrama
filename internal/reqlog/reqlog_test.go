package reqlog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 10, 9, 30, 0, 0, time.Local)
	for i, e := range []*Entry{
		{Time: base, Kind: KindLLM, Stage: "idea", Status: "ok", Request: map[string]any{"user": "hello"}},
		{Time: base.Add(time.Second), Kind: KindImage, Stage: "Key Frames!", Status: "error", Error: "boom"},
		{Time: base.Add(24 * time.Hour), Kind: KindLLM, Stage: "storyboard", Status: "ok"},
	} {
		if err := s.Write(e); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	days, err := s.Days()
	if err != nil || len(days) != 2 || days[0].Date != "2026-11-11" || days[1].Count != 2 || days[1].Size == 0 {
		t.Fatalf("days: %+v %v", days, err)
	}
	list, _ := s.List("2026-11-10")
	if len(list) != 2 || list[0].Kind != KindImage || list[1].Request["user"] != "hello" {
		t.Fatalf("list not newest first: %+v", list)
	}
	if list[0].Name != "093001.000_image_key-frames_"+list[0].Name[len(list[0].Name)-8:] {
		t.Errorf("unexpected file name %q", list[0].Name)
	}
	got, err := s.Get(list[0].Day, list[0].Name)
	if err != nil || got.Error != "boom" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, got.Day, got.Name+".json")); info.Mode().Perm() != 0o600 {
		t.Errorf("log file mode %v", info.Mode())
	}

	// References are validated: no path traversal.
	for _, ref := range [][2]string{{"..", "x"}, {"2026-11-10", "../../etc/passwd"}, {"2026-11-10", "missing"}} {
		if _, err := s.Get(ref[0], ref[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q, %q) = %v, want ErrNotFound", ref[0], ref[1], err)
		}
	}
	if err := s.DeleteDay("../x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteDay traversal: %v", err)
	}

	if err := s.DeleteDay("2026-11-10"); err != nil {
		t.Fatal(err)
	}
	if days, _ := s.Days(); len(days) != 1 {
		t.Fatalf("after delete: %+v", days)
	}
	if err := s.DeleteAll(); err != nil {
		t.Fatal(err)
	}
	if days, _ := s.Days(); len(days) != 0 {
		t.Fatalf("after delete all: %+v", days)
	}

	// A nil store discards writes.
	var none *Store
	if err := none.Write(&Entry{}); err != nil {
		t.Error(err)
	}
	if l, _ := none.List("2026-11-10"); l != nil {
		t.Error("nil store should list nothing")
	}
}

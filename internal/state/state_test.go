package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMissingFile(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("nope"); ok {
		t.Error("empty store should have no entries")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := StackState{
		LastDeployedSHA: "abc123", LastStatus: StatusSuccess,
		LastRunID: "20260718-120000-ff00", UpdatedAt: time.Now().Truncate(time.Second),
	}
	if err := s.Put("webapp", want); err != nil {
		t.Fatal(err)
	}

	// reopen: state must survive a restart
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("webapp")
	if !ok {
		t.Fatal("webapp missing after reload")
	}
	if got.LastDeployedSHA != want.LastDeployedSHA || got.LastStatus != want.LastStatus {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPutIsAtomicNoTempLeftovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Put("a", StackState{LastDeployedSHA: "sha", UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("leftover files: %v", names)
	}
}

func TestOpenCorruptedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("want error on corrupted state file")
	}
}

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

// A decommissioned stack keeps a tombstone while it is still in config.toml
// (so the scheduler cannot redeploy it), and the tombstone is dropped once the
// stack is gone from config. Ordinary entries are never touched.
func TestPruneDecommissioned(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Put("gone", StackState{Decommissioned: true}))
	must(s.Put("kept", StackState{Decommissioned: true}))
	must(s.Put("orphan", StackState{LastDeployedSHA: "abc"}))

	inConfig := map[string]bool{"kept": true}
	must(s.PruneDecommissioned(func(name string) bool { return inConfig[name] }))

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("gone"); ok {
		t.Error("tombstone of a stack removed from config must be pruned")
	}
	if st, ok := reopened.Get("kept"); !ok || !st.Decommissioned {
		t.Error("tombstone of a stack still in config must be kept")
	}
	if _, ok := reopened.Get("orphan"); !ok {
		t.Error("non-decommissioned entries must not be pruned")
	}
}

func TestIsDecommissioned(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("gone", StackState{Decommissioned: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("live", StackState{LastDeployedSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	if !s.IsDecommissioned("gone") {
		t.Error("tombstoned stack must be reported as decommissioned")
	}
	if s.IsDecommissioned("live") || s.IsDecommissioned("unknown") {
		t.Error("ordinary and unknown stacks must not be reported as decommissioned")
	}
}

package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreRoundTripPreservesFields(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	store.Configure(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	store.Mutate(func(s *State) {
		s.TaskSchedules["keepalive"] = []int{3, 22}
		s.PromptMode = "append"
		s.DisabledModels = []string{"vendor:model"}
		s.ModelAliases = map[string]string{"vendor:model": "alias"}
		s.AccountAliases = map[string]string{"workbuddycn-u1": "工作号"}
	})

	reloaded := NewStore()
	reloaded.Configure(dir)
	got, err := reloaded.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.TaskSchedules["keepalive"]) != 2 || got.PromptMode != "append" {
		t.Fatalf("settings lost: %#v", got)
	}
	if len(got.DisabledModels) != 1 || got.ModelAliases["vendor:model"] != "alias" {
		t.Fatalf("model fields lost: %#v", got)
	}
	if got.AccountAliases["workbuddycn-u1"] != "工作号" {
		t.Fatalf("account aliases lost: %#v", got.AccountAliases)
	}
	raw, errRead := os.ReadFile(filepath.Join(dir, "state.json"))
	if errRead != nil || len(raw) == 0 {
		t.Fatalf("state file missing: %v", errRead)
	}
}

func TestStoreCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	store.Configure(dir)
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load corrupt: %v", err)
	}
	if got.Claim == nil || got.TaskSchedules == nil || got.ModelAliases == nil || got.AccountAliases == nil {
		t.Fatalf("empty state not normalized: %#v", got)
	}
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	store := NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	store.Mutate(func(s *State) {
		s.DisabledModels = []string{"a"}
		s.AccountAliases["f-1"] = "工作号"
	})
	snap := store.Snapshot()
	snap.DisabledModels[0] = "changed"
	snap.AccountAliases["f-1"] = "changed"
	if got := store.Snapshot(); got.DisabledModels[0] != "a" || got.AccountAliases["f-1"] != "工作号" {
		t.Fatalf("snapshot aliases internal state: %#v", got)
	}
}

func TestMutateIsConcurrentSafe(t *testing.T) {
	store := NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Mutate(func(s *State) { s.AccountAliases["k"] = "v" })
			_ = store.Snapshot()
		}()
	}
	wg.Wait()
}

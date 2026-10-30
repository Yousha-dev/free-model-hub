package auth

import (
	"free-model-hub/internal/core"
	"sync"
	"testing"
	"time"
)

var testRegistry = NewSessionRegistry()

func TestRegisterPollOwnsDelete(t *testing.T) {
	testRegistry.Register("vendor-a", "sess-1", time.Minute, 0, "payload")

	session, state := testRegistry.Poll("sess-1")
	if state != core.PollReady {
		t.Fatalf("poll state = %v, want ready", state)
	}
	if session == nil || session.VendorID != "vendor-a" || session.Data != "payload" {
		t.Fatalf("session = %+v", session)
	}

	vendorID, okOwns := testRegistry.Owns("sess-1")
	if !okOwns || vendorID != "vendor-a" {
		t.Fatalf("owns = %q %v", vendorID, okOwns)
	}

	testRegistry.Delete("sess-1")
	if _, stateMissing := testRegistry.Poll("sess-1"); stateMissing != core.PollMissing {
		t.Fatalf("after delete poll state = %v", stateMissing)
	}
	if _, okAfter := testRegistry.Owns("sess-1"); okAfter {
		t.Fatal("deleted session must not be owned")
	}
}

func TestPollThrottling(t *testing.T) {
	testRegistry.Register("vendor-b", "sess-2", time.Minute, time.Hour, nil)

	if _, state := testRegistry.Poll("sess-2"); state != core.PollReady {
		t.Fatalf("first poll = %v, want ready", state)
	}
	if _, state := testRegistry.Poll("sess-2"); state != core.PollThrottled {
		t.Fatalf("second poll = %v, want throttled", state)
	}
}

func TestPollExpiry(t *testing.T) {
	testRegistry.Register("vendor-c", "sess-3", time.Millisecond, 0, nil)
	time.Sleep(5 * time.Millisecond)

	if _, state := testRegistry.Poll("sess-3"); state != core.PollMissing {
		t.Fatalf("expired poll = %v, want missing", state)
	}
	if _, okOwns := testRegistry.Owns("sess-3"); okOwns {
		t.Fatal("expired session must not be owned")
	}
}

func TestPollMissingAndEmptyID(t *testing.T) {
	if _, state := testRegistry.Poll(""); state != core.PollMissing {
		t.Fatalf("empty id state = %v", state)
	}
	if _, state := testRegistry.Poll("nope"); state != core.PollMissing {
		t.Fatalf("unknown id state = %v", state)
	}
	// Empty IDs cannot be registered to prevent all empty-string sessions from overwriting each other.
	testRegistry.Register("vendor-d", "  ", time.Minute, 0, nil)
	if _, okOwns := testRegistry.Owns(""); okOwns {
		t.Fatal("empty id must not be registered")
	}
}

func TestNewID(t *testing.T) {
	first := core.RandomHex(8)
	second := core.RandomHex(8)
	if len(first) != 16 {
		t.Fatalf("NewID length = %d, want 16", len(first))
	}
	if first == second {
		t.Fatal("NewID must produce distinct ids")
	}
	if got := core.RandomHex(0); len(got) != 32 {
		t.Fatalf("default NewID length = %d, want 32", len(got))
	}
}

func TestConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				id := core.RandomHex(4)
				testRegistry.Register("vendor-e", id, time.Minute, 0, nil)
				_, _ = testRegistry.Poll(id)
				_, _ = testRegistry.Owns(id)
				testRegistry.Delete(id)
			}
		}()
	}
	wg.Wait()
}

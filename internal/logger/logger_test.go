package logger

import (
	"fmt"
	"sync"
	"testing"
)

func TestRingKeepsNewestEntries(t *testing.T) {
	r := &ring{entries: make([]Entry, 0, ringCapacity)}
	for i := 0; i < ringCapacity+25; i++ {
		r.append(Entry{Message: fmt.Sprintf("entry-%d", i)})
	}
	ordered := r.orderedEntries()
	if len(ordered) != ringCapacity {
		t.Fatalf("ring size = %d, want %d", len(ordered), ringCapacity)
	}
	if ordered[0].Message != "entry-25" {
		t.Fatalf("oldest = %q, want entry-25", ordered[0].Message)
	}
	if ordered[len(ordered)-1].Message != fmt.Sprintf("entry-%d", ringCapacity+24) {
		t.Fatalf("newest = %q", ordered[len(ordered)-1].Message)
	}
}

func TestSnapshotSinceAndLimit(t *testing.T) {
	r := &ring{entries: make([]Entry, 0, ringCapacity)}
	globalRing = r
	for i := 0; i < 10; i++ {
		r.append(Entry{Message: fmt.Sprintf("m%d", i)})
	}
	entries, newest := Snapshot(5, 3)
	if newest != 10 {
		t.Fatalf("newest = %d", newest)
	}
	if len(entries) != 3 || entries[0].Seq != 6 || entries[2].Seq != 8 {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestRingConcurrentAppend(t *testing.T) {
	r := &ring{entries: make([]Entry, 0, ringCapacity)}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.append(Entry{Message: fmt.Sprintf("%d-%d", id, j)})
			}
		}(i)
	}
	wg.Wait()
	if got := len(r.orderedEntries()); got != 1000 {
		t.Fatalf("entries = %d", got)
	}
}

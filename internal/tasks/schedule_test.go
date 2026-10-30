package tasks

import (
	"testing"
	"time"
)

func TestNormalizeHoursAndNextWake(t *testing.T) {
	if got := NormalizeHours([]int{9, 9, 25, -1, 21}); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("NormalizeHours = %#v", got)
	}
}

func TestNormalizeHoursKeepsValidValues(t *testing.T) {
	if got := NormalizeHours([]int{0, 23}); len(got) != 2 || got[0] != 0 || got[1] != 23 {
		t.Fatalf("NormalizeHours = %#v", got)
	}
}

func TestNextWakePicksNextHourOrTomorrow(t *testing.T) {
	now := time.Date(2026, 10, 29, 21, 30, 0, 0, time.UTC)
	if got := NextWake(now, []int{22}); !got.Equal(time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextWake same day = %v", got)
	}
	if got := NextWake(now, []int{9}); !got.Equal(time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextWake tomorrow = %v", got)
	}
}

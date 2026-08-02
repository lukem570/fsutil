package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFDBudgetAccounting(t *testing.T) {
	b := &fdBudget{budget: 3}

	for i := range 3 {
		if !b.acquire() {
			t.Fatalf("acquire %d was refused within budget", i)
		}
	}
	if b.acquire() {
		t.Fatal("acquire succeeded beyond the budget")
	}

	held, budget, denied, _ := b.snapshot()
	if held != 3 || budget != 3 || denied != 1 {
		t.Errorf("snapshot = (held %d, budget %d, denied %d), want (3, 3, 1)", held, budget, denied)
	}

	b.release()
	if !b.acquire() {
		t.Error("acquire was refused after a release freed room")
	}
}

// TestFDBudgetReserveAlwaysSucceeds checks the rule that keeps the budget from
// becoming a new failure mode: a path the caller explicitly asked to watch is
// watched, whatever the accounting says.
func TestFDBudgetReserveAlwaysSucceeds(t *testing.T) {
	b := &fdBudget{budget: 1}

	for range 5 {
		b.reserve()
	}
	if held, _, _, _ := b.snapshot(); held != 5 {
		t.Errorf("held = %d after 5 reserves, want 5", held)
	}
	// Going over budget must remain visible rather than being quietly clamped.
	if held, budget, _, _ := b.snapshot(); held <= budget {
		t.Errorf("held %d is not above budget %d; exceeding it should be observable", held, budget)
	}
}

func TestFDBudgetReleaseDoesNotUnderflow(t *testing.T) {
	b := &fdBudget{budget: 2}
	b.release()
	b.release()
	if held, _, _, _ := b.snapshot(); held != 0 {
		t.Errorf("held = %d after releasing more than was acquired, want 0", held)
	}
}

func TestFDBudgetDerivation(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		wantMin    int
	}{
		{"configured value is honoured", 7, 7},
		{"derived from the process limit", 0, fdMinimumBudget},
	}
	for _, tt := range tests {
		b := newFDBudget(tt.configured)
		_, budget, _, _ := b.snapshot()
		if tt.configured != 0 && budget != tt.configured {
			t.Errorf("%s: budget = %d, want %d", tt.name, budget, tt.configured)
		}
		if budget < tt.wantMin {
			t.Errorf("%s: budget = %d, want at least %d", tt.name, budget, tt.wantMin)
		}
	}
}

// TestFDBudgetLeavesHeadroom checks that the watcher cannot consume every
// descriptor in the process.
//
// This is the failure the budget exists to prevent, and it is the nastier of
// the two possible outcomes: a watcher short of budget merely reports less,
// whereas a watcher that has taken every descriptor breaks unrelated code
// somewhere else in the program, where nobody will think to look for it.
func TestFDBudgetLeavesHeadroom(t *testing.T) {
	b := newFDBudget(0)
	_, budget, _, limit := b.snapshot()

	if limit == 0 {
		t.Skip("this platform does not report a descriptor limit")
	}
	if budget >= limit {
		t.Errorf("budget %d leaves no headroom below the process limit %d", budget, limit)
	}
	t.Logf("process limit %d, watcher budget %d, headroom %d", limit, budget, limit-budget)
}

// TestWatchMoreFilesThanTheDescriptorLimit is the headline test for this
// milestone.
//
// A watcher must not make the number of watchable files a function of the
// process's descriptor limit. On backends that spend a descriptor per file
// that would once have been an inescapable ceiling; the budget turns it into a
// loss of precision instead, so the files are still watched and their
// appearance and removal still reported.
//
// The test is meaningful in proportion to how the platform watches. Where a
// directory costs one descriptor no matter how many files it holds, it passes
// comfortably; where each file costs one, it exercises exactly the degradation
// path this milestone added. It runs everywhere so that CI covers both.
func TestWatchMoreFilesThanTheDescriptorLimit(t *testing.T) {
	const files = 2000

	limit := raiseFDLimit()
	t.Logf("process descriptor limit: %d", limit)

	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		for i := range files {
			writeFile(t, filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), "x")
		}

		// A budget far below the number of files forces the degradation path
		// on any backend that has one, without needing to lower the process
		// limit and destabilise the test binary itself.
		w, c := newTestWatcherOn(t, kind, WithFDBudget(64))
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add with %d files present: %s", files, err)
		}

		stats := w.Stats()
		t.Logf("%s: %s", kind, stats)

		// Whatever the accounting, creation and removal must still be
		// reported: they are visible from the directory alone.
		created := filepath.Join(dir, "brand-new.txt")
		writeFile(t, created, "content")
		c.await(t, "a creation in a directory of 2000 files", func(evs []Event) bool {
			return hasEvent(evs, created, Create)
		})

		if err := os.Remove(created); err != nil {
			t.Fatalf("Remove: %s", err)
		}
		c.await(t, "a removal in a directory of 2000 files", func(evs []Event) bool {
			return hasEvent(evs, created, Remove)
		})

		if stats.DescriptorsDenied > 0 {
			t.Logf("%s degraded as designed: %d descriptors refused, so modification "+
				"events for some files are not reported", kind, stats.DescriptorsDenied)
		}
	})
}

func TestStatsOnAFreshWatcher(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		w, _ := newTestWatcherOn(t, kind)

		if got := w.Stats().Watches; got != 0 {
			t.Errorf("Stats().Watches on a fresh watcher = %d, want 0", got)
		}

		dir := t.TempDir()
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}
		if got := w.Stats().Watches; got != 1 {
			t.Errorf("Stats().Watches after one Add = %d, want 1", got)
		}
	})
}

func TestStatsAfterCloseIsEmpty(t *testing.T) {
	w, err := NewWatcherWith(WithBackend(BackendPoll), WithPollInterval(testPollInterval))
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	collect(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}
	if got := w.Stats(); got != (Stats{}) {
		t.Errorf("Stats() after Close = %+v, want the zero value", got)
	}
}

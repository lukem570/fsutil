package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestNativeAgreesWithPolling checks a native backend against the polling one
// on the same tree at the same time.
//
// Polling is the oracle here, and deliberately so: it is the only backend that
// decides what happened by looking at the filesystem rather than by being told,
// so it cannot be wrong in the same way a misread kernel event can. If a
// kernel-driven backend disagrees with what a before-and-after comparison
// shows, the kernel-driven one is the suspect.
//
// The comparison is deliberately one-directional. Polling misses anything that
// begins and ends between two scans, so it can report less than a native
// backend and be right. What it must not do is report a durable change that
// the native backend missed entirely — that is the failure worth catching,
// because it is the one that looks like a quiet filesystem.
func TestNativeAgreesWithPolling(t *testing.T) {
	native := nativeBackend(t)

	dir := t.TempDir()

	oracle, oracleEvents := newTestWatcherOn(t, BackendPoll)
	subject, subjectEvents := newTestWatcherOn(t, native)

	for _, w := range []*Watcher{oracle, subject} {
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}
	}

	// Durable changes only: each file is created and left alone, so nothing
	// depends on either backend catching a transient state.
	var created []string
	for i := range 12 {
		path := filepath.Join(dir, fmt.Sprintf("f%02d.txt", i))
		writeFile(t, path, "content")
		created = append(created, path)
	}

	// Wait for the slower of the two to have seen everything it is going to.
	oracleEvents.await(t, "polling to observe every creation", func(evs []Event) bool {
		for _, path := range created {
			if !hasEvent(evs, path, Create) {
				return false
			}
		}
		return true
	})
	subjectEvents.await(t, native.String()+" to observe every creation", func(evs []Event) bool {
		for _, path := range created {
			if !hasEvent(evs, path, Create) {
				return false
			}
		}
		return true
	})

	// Now remove half of them, which is the case where the two backends reach
	// their answer most differently: one is told, the other notices an absence.
	var removed []string
	for i, path := range created {
		if i%2 == 0 {
			continue
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove: %s", err)
		}
		removed = append(removed, path)
	}

	oracleEvents.await(t, "polling to observe every removal", func(evs []Event) bool {
		for _, path := range removed {
			if !hasEvent(evs, path, Remove) {
				return false
			}
		}
		return true
	})

	// Everything polling saw by comparing the filesystem, the native backend
	// must also have reported.
	missing := disagreements(oracleEvents.snapshot(), subjectEvents.snapshot())
	if len(missing) > 0 {
		t.Errorf("%s missed %d durable change(s) that polling observed:\n%s",
			native, len(missing), formatEvents(missing))
	}
}

// nativeBackend returns a backend that hears from the kernel, or skips.
func nativeBackend(t *testing.T) Backend {
	t.Helper()

	for _, kind := range Backends() {
		if kind == BackendPoll {
			continue
		}
		// A privileged backend is available here only by accident of how the
		// tests are run, and comparing against it proves nothing about how the
		// library behaves for anyone else.
		if factoryCaps[kind].Has(CapPrivileged) {
			continue
		}
		return kind
	}
	t.Skip("no unprivileged kernel-driven backend on this host")
	return BackendAuto
}

// disagreements returns the durable changes present in want but absent from
// got.
//
// Only creations and removals are compared. Writes are excluded because the
// two backends legitimately count them differently — one reports each write
// syscall, the other reports that a file differs from how it last looked — and
// a difference there is not evidence of anything.
func disagreements(want, got []Event) []Event {
	seen := make(map[string]Op, len(got))
	for _, ev := range got {
		seen[ev.Name] |= ev.Op
	}

	var missing []Event
	for _, ev := range want {
		durable := ev.Op & (Create | Remove)
		if durable == 0 {
			continue
		}
		if seen[ev.Name]&durable == 0 {
			missing = append(missing, Event{Name: ev.Name, Op: durable})
		}
	}
	return missing
}

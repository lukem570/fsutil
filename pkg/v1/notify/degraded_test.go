package notify

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The poller's behaviour is independent of which backend gave up on a file, so
// it is tested directly rather than only through the one platform that
// currently degrades. tick is driven by hand, which makes every assertion
// deterministic instead of a race against an interval.

func newTestPoller(t *testing.T) (*degradedPoller, *collector, chan Event, chan error) {
	t.Helper()

	events := make(chan Event, 64)
	errs := make(chan error, 8)
	done := make(chan struct{})

	p := newDegradedPoller(chanSink{events: events, errors: errs, done: done}, time.Hour)
	t.Cleanup(func() {
		p.close()
		close(done)
	})

	return p, nil, events, errs
}

func drainEvents(ch chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestDegradedReportsWrites(t *testing.T) {
	p, _, events, _ := newTestPoller(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "one")

	p.add(file, portableOps)

	// The file as it already is must not be reported: adding a file to be
	// compared is not a change to it.
	p.tick()
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("reported %d event(s) for an unchanged file:\n%s", len(got), formatEvents(got))
	}

	writeFile(t, file, "one and rather more")
	p.tick()

	got := drainEvents(events)
	if !hasEvent(got, file, Write) {
		t.Fatalf("no write reported after modifying the file:\n%s", formatEvents(got))
	}
}

func TestDegradedReportsChmod(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, where permissions do not constrain anything")
	}

	p, _, events, _ := newTestPoller(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "content")

	p.add(file, portableOps)
	p.tick()
	drainEvents(events)

	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatalf("Chmod: %s", err)
	}
	p.tick()

	if got := drainEvents(events); !hasEvent(got, file, Chmod) {
		t.Errorf("no chmod reported after changing permissions:\n%s", formatEvents(got))
	}
}

// A removal is reported by the watch on the containing directory, which is the
// only place that can tell it from a rename. Reporting it here as well would
// duplicate it, and reporting it here as a removal when it was a rename would
// be wrong.
func TestDegradedSaysNothingAboutRemoval(t *testing.T) {
	p, _, events, _ := newTestPoller(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "content")

	p.add(file, portableOps)
	p.tick()
	drainEvents(events)

	if err := os.Remove(file); err != nil {
		t.Fatalf("Remove: %s", err)
	}
	p.tick()

	if got := drainEvents(events); len(got) != 0 {
		t.Errorf("reported %d event(s) for a removal the directory already covers:\n%s",
			len(got), formatEvents(got))
	}
	if p.watching() != 0 {
		t.Errorf("still comparing %d file(s) after one was removed, want 0", p.watching())
	}
}

func TestDegradedHonoursOpFilter(t *testing.T) {
	p, _, events, _ := newTestPoller(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "one")

	// A watch that never asked for writes must not receive them merely
	// because this is where the file ended up being observed.
	p.add(file, Create|Remove)
	if p.watching() != 0 {
		t.Fatalf("comparing a file whose watch wants nothing this can report")
	}

	writeFile(t, file, "one and rather more")
	p.tick()
	if got := drainEvents(events); len(got) != 0 {
		t.Errorf("reported %d event(s) on a watch restricted to creates and removes:\n%s",
			len(got), formatEvents(got))
	}
}

// The goroutine exists only while there is something to compare. A backend
// that briefly exceeded its budget and recovered must not leave a timer
// running for the life of the process.
func TestDegradedRunsOnlyWhileNeeded(t *testing.T) {
	p, _, _, _ := newTestPoller(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "content")

	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if running {
		t.Error("the poller was running before it had anything to compare")
	}

	p.add(file, portableOps)
	p.mu.Lock()
	running = p.running
	p.mu.Unlock()
	if !running {
		t.Error("the poller did not start when given a file")
	}

	p.remove(file)
	p.mu.Lock()
	running = p.running
	p.mu.Unlock()
	if running {
		t.Error("the poller kept running with nothing left to compare")
	}
}

func TestDegradedCloseIsIdempotent(t *testing.T) {
	p, _, _, _ := newTestPoller(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "content")
	p.add(file, portableOps)

	for range 3 {
		p.close()
	}
	if p.watching() != 0 {
		t.Errorf("still comparing %d file(s) after close", p.watching())
	}
	// Adding after close must not restart anything.
	p.add(file, portableOps)
	if p.watching() != 0 {
		t.Errorf("accepted a file after close")
	}
}

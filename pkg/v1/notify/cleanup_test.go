package notify

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// newAbandonedWatcher creates a watcher, starts it watching dir, and returns
// only its channels — deliberately dropping every reference to the Watcher
// itself.
//
// It is a separate function, and one the compiler may not inline, so that the
// Watcher cannot remain live in the caller's stack frame. Inlining it into a
// test would leave the receiver reachable and the cleanup would never run,
// which would make the test pass or fail for reasons unrelated to the code
// under test.
//
//go:noinline
func newAbandonedWatcher(t *testing.T, kind Backend, dir string) (chan Event, chan error) {
	t.Helper()

	w, err := NewWatcherWith(
		WithBackend(kind),
		WithPollInterval(testPollInterval),
		WithEventBuffer(64),
	)
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}
	return w.Events, w.Errors
}

// awaitCollection repeatedly forces a collection until ch is closed.
//
// A cleanup runs only after its object has actually been collected, and one
// cycle is not always enough to get there, so this drives the collector rather
// than waiting for it.
func awaitCollection(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()

	deadline := time.Now().Add(testTimeout)
	for {
		runtime.GC()

		select {
		case <-ch:
			return
		case <-time.After(5 * time.Millisecond):
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestAbandonedWatcherIsCleanedUp is the leak backstop.
//
// A watcher dropped without Close must not keep its backend running for the
// life of the process. The observable consequence of cleanup is that the event
// channels are closed, which is what a consumer ranging over them sees.
func TestAbandonedWatcherIsCleanedUp(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		events, errs := newAbandonedWatcher(t, kind, dir)

		eventsClosed := make(chan struct{})
		go func() {
			for range events { //revive:disable-line:empty-block
			}
			close(eventsClosed)
		}()

		errsClosed := make(chan struct{})
		go func() {
			for range errs { //revive:disable-line:empty-block
			}
			close(errsClosed)
		}()

		// Keep the filesystem changing, so a watcher that survived collection
		// has something to report and the test fails loudly rather than by
		// timeout.
		go func() {
			for i := range 100 {
				select {
				case <-eventsClosed:
					return
				default:
				}
				writeFileNoFatal(filepath.Join(dir, "churn.txt"), i)
				time.Sleep(testPollInterval)
			}
		}()

		awaitCollection(t, "the abandoned watcher's Events channel to close", eventsClosed)
		awaitCollection(t, "the abandoned watcher's Errors channel to close", errsClosed)
	})
}

// TestAbandonedWatcherStopsItsGoroutines checks that cleanup releases the
// backend's goroutines, not merely its channels. Closing the channels while
// leaving a scan loop running would still leak a thread of execution and,
// with it, every descriptor the backend holds.
func TestAbandonedWatcherStopsItsGoroutines(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()

		before := runtime.NumGoroutine()

		events, errs := newAbandonedWatcher(t, kind, dir)
		closed := make(chan struct{})
		go func() {
			for range events { //revive:disable-line:empty-block
			}
			close(closed)
		}()
		go func() {
			for range errs { //revive:disable-line:empty-block
			}
		}()

		awaitCollection(t, "the abandoned watcher to shut down", closed)

		// The two draining goroutines above exit once the channels close, but
		// not necessarily before this check, so allow for them settling.
		deadline := time.Now().Add(testTimeout)
		for {
			if runtime.NumGoroutine() <= before+1 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutines not released: %d before, %d after",
					before, runtime.NumGoroutine())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// TestCloseThenCollectDoesNotPanic guards the interaction between the two
// shutdown paths.
//
// Closing the channels twice panics, so an explicit Close followed by
// collection of the same watcher must not run the teardown a second time.
func TestCloseThenCollectDoesNotPanic(t *testing.T) {
	dir := t.TempDir()

	func() {
		w, err := NewWatcherWith(
			WithBackend(BackendPoll),
			WithPollInterval(testPollInterval),
		)
		if err != nil {
			t.Fatalf("NewWatcherWith: %s", err)
		}
		collect(t, w)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %s", err)
		}
	}()

	// If Close failed to cancel the cleanup, this is where the double close
	// would surface.
	for range 5 {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCleanupDoesNotFireOnLiveWatcher checks the other half of the contract:
// the cleanup must not reference the Watcher, or it would keep it alive
// forever; but it must also not fire while the Watcher is still in use.
func TestCleanupDoesNotFireOnLiveWatcher(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWatcher(t)
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}

	// Collect aggressively while the watcher is still referenced.
	for range 5 {
		runtime.GC()
		time.Sleep(2 * time.Millisecond)
	}

	file := filepath.Join(dir, "still-working.txt")
	writeFile(t, file, "content")
	c.await(t, "an event from a watcher that is still referenced", func(evs []Event) bool {
		return hasEvent(evs, file, Create)
	})

	runtime.KeepAlive(w)
}

// writeFileNoFatal writes without touching *testing.T, so it is safe to call
// from a goroutine the test does not join.
func writeFileNoFatal(path string, n int) {
	content := make([]byte, n+1)
	for i := range content {
		content[i] = 'x'
	}
	_ = writeFileBytes(path, content)
}

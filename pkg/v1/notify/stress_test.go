package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// The tests here target failures that a correctness-focused suite misses
// because they need volume, churn, or timing to appear: resources that are
// acquired but never released, and events that arrive for watches that have
// gone.
//
// They matter most for the backends that cannot be run on the machine they
// were written on. Both kqueue and the Windows backend hold a kernel resource
// per watch and both must cope with notifications that outlive the watch they
// belong to, which is precisely the kind of defect that a single-watch test
// will never provoke and a leak test will catch on the first CI run.

// openDescriptors reports how many descriptors this process holds, or false
// where that cannot be determined without leaving pure Go.
func openDescriptors() (int, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	// The directory handle opened to read this is itself counted, but it is
	// counted identically in both the before and after measurements, so it
	// cancels out.
	return len(entries), true
}

// TestNoDescriptorLeakUnderChurn is the leak detector.
//
// A backend that spends a descriptor per watch and forgets to release one on
// some path — an error branch, a watch replaced by re-adding, a directory that
// vanished between opening and registering — leaks silently until the process
// runs out. Adding and removing the same watches repeatedly turns a slow leak
// into an obvious one.
func TestNoDescriptorLeakUnderChurn(t *testing.T) {
	const rounds = 50

	eachBackend(t, func(t *testing.T, kind Backend) {
		if _, ok := openDescriptors(); !ok {
			t.Skip("this platform does not expose its open descriptors")
		}

		dirs := make([]string, 8)
		for i := range dirs {
			dirs[i] = t.TempDir()
		}

		w, _ := newTestWatcherOn(t, kind)

		// Settle first: the first Add allocates whatever the backend needs
		// once, and counting that as a leak would be wrong.
		for _, dir := range dirs {
			if err := w.Add(dir); err != nil {
				t.Fatalf("Add: %s", err)
			}
		}
		for _, dir := range dirs {
			if err := w.Remove(dir); err != nil {
				t.Fatalf("Remove: %s", err)
			}
		}

		before, _ := openDescriptors()

		for range rounds {
			for _, dir := range dirs {
				if err := w.Add(dir); err != nil {
					t.Fatalf("Add: %s", err)
				}
			}
			for _, dir := range dirs {
				if err := w.Remove(dir); err != nil {
					t.Fatalf("Remove: %s", err)
				}
			}
		}

		after, _ := openDescriptors()

		// A little slack absorbs runtime allocations unrelated to watching. A
		// genuine leak of one descriptor per add would show up as hundreds.
		if slack := 16; after > before+slack {
			t.Errorf("descriptors grew from %d to %d over %d add/remove rounds across %d directories; "+
				"a per-watch resource is not being released",
				before, after, rounds, len(dirs))
		} else {
			t.Logf("descriptors: %d before, %d after %d rounds", before, after, rounds)
		}
	})
}

// TestManyWatches checks that a realistic number of watches can be held at
// once, and released together.
//
// The number is chosen to exceed a typical default descriptor limit on the
// platforms that spend one per watch, so that the descriptor budget and the
// error translation both get exercised rather than merely existing.
func TestManyWatches(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	const watches = 400

	eachBackend(t, func(t *testing.T, kind Backend) {
		root := t.TempDir()
		dirs := make([]string, watches)
		for i := range dirs {
			dir := filepath.Join(root, fmt.Sprintf("d%04d", i))
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatalf("Mkdir: %s", err)
			}
			dirs[i] = dir
		}

		w, c := newTestWatcherOn(t, kind)
		for _, dir := range dirs {
			if err := w.Add(dir); err != nil {
				t.Fatalf("Add after %d watches: %s", len(w.WatchList()), err)
			}
		}

		if got := len(w.WatchList()); got != watches {
			t.Fatalf("WatchList() has %d entries, want %d", got, watches)
		}
		t.Logf("%s: %s", kind, w.Stats())

		// The last watch added must work as well as the first. A backend that
		// silently stops registering once a limit is reached would still pass
		// every test above this one.
		last := filepath.Join(dirs[watches-1], "probe.txt")
		writeFile(t, last, "content")
		c.await(t, "an event from the last of many watches", func(evs []Event) bool {
			return hasEvent(evs, last, Create)
		})

		first := filepath.Join(dirs[0], "probe.txt")
		writeFile(t, first, "content")
		c.await(t, "an event from the first of many watches", func(evs []Event) bool {
			return hasEvent(evs, first, Create)
		})
	})
}

// TestRemoveWhileEventsAreInFlight targets the stale-notification bug class.
//
// Removing a watch does not recall the notifications already queued for it.
// Those arrive afterwards, referring to a watch that no longer exists, and a
// backend that trusts them can act on a kernel resource whose identifier has
// since been reissued to something else. The consequences range from an event
// attributed to the wrong path to closing a handle another part of the program
// owns.
//
// Generating changes and removing the watch underneath them is how that window
// is made wide enough to hit.
func TestRemoveWhileEventsAreInFlight(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		w, _ := newTestWatcherOn(t, kind)

		for round := range 20 {
			dir := t.TempDir()
			if err := w.Add(dir); err != nil {
				t.Fatalf("round %d: Add: %s", round, err)
			}

			// Produce changes and pull the watch out from under them.
			for i := range 10 {
				writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), "content")
			}
			if err := w.Remove(dir); err != nil {
				t.Fatalf("round %d: Remove: %s", round, err)
			}

			// Keep changing the directory after the watch is gone: a backend
			// that re-armed a removed watch would pick these up.
			for i := range 10 {
				_ = os.Remove(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)))
			}
		}

		// Surviving is most of the point — this is the shape of failure that
		// takes the process down rather than failing an assertion — but the
		// watcher must also still work afterwards.
		dir := t.TempDir()
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add after churn: %s", err)
		}
		if got := w.WatchList(); len(got) != 1 {
			t.Errorf("WatchList() = %v after churn, want exactly one entry", got)
		}
	})
}

// TestCloseWhileEventsAreArriving checks shutdown against a moving target.
//
// Closing while the kernel still has things to say is the normal case, not an
// edge one: a program shutting down rarely pauses the filesystem first.
func TestCloseWhileEventsAreArriving(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()

		w, err := NewWatcherWith(WithBackend(kind), WithPollInterval(testPollInterval))
		if err != nil {
			t.Fatalf("NewWatcherWith: %s", err)
		}
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}

		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = writeFileBytes(filepath.Join(dir, fmt.Sprintf("f%d.txt", i%50)), []byte("x"))
			}
		}()

		// Consume for a moment, then stop reading entirely, so that Close has
		// to contend with a backend parked mid-delivery.
		go func() {
			for range w.Events { //revive:disable-line:empty-block
			}
		}()
		go func() {
			for range w.Errors { //revive:disable-line:empty-block
			}
		}()

		time.Sleep(50 * time.Millisecond)

		closed := make(chan error, 1)
		go func() { closed <- w.Close() }()

		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("Close: %s", err)
			}
		case <-time.After(testTimeout):
			t.Fatal("Close blocked while events were arriving")
		}

		close(stop)
		<-done
	})
}

// TestGoroutinesReleasedOnClose checks that closing a watcher leaves nothing
// running.
//
// A backend whose reader goroutine survives Close holds every resource that
// goroutine references, so a program creating watchers in a loop accumulates
// them without any single Close appearing to fail.
func TestGoroutinesReleasedOnClose(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		before := runtime.NumGoroutine()

		for range 20 {
			w, err := NewWatcherWith(WithBackend(kind), WithPollInterval(testPollInterval))
			if err != nil {
				t.Fatalf("NewWatcherWith: %s", err)
			}
			c := collect(t, w)
			if err := w.Add(t.TempDir()); err != nil {
				t.Fatalf("Add: %s", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %s", err)
			}
			<-c.closed
		}

		deadline := time.Now().Add(testTimeout)
		for {
			if now := runtime.NumGoroutine(); now <= before+2 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutines grew from %d to %d over 20 watcher lifetimes",
					before, runtime.NumGoroutine())
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

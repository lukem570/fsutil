package notify

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The conformance suite is the definition of correct behaviour for a backend.
//
// Every backend runs every test here. That is the point: the value of a
// cross-platform notification library is that a program written against it
// behaves the same everywhere, and the only way to know that is to hold each
// implementation to one shared description rather than to a set of
// platform-specific tests that were each written to match whatever the
// implementation already did.
//
// Where a backend genuinely cannot do something, it says so through a
// [Capability] and the relevant test skips. A skip is therefore a documented
// difference between platforms, visible in the test output, rather than a gap
// nobody noticed.

// eachBackend runs fn as a subtest against every backend usable on this host.
func eachBackend(t *testing.T, fn func(t *testing.T, kind Backend)) {
	t.Helper()

	kinds := Backends()
	if len(kinds) == 0 {
		t.Fatal("no backends available; polling should always be usable")
	}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) { fn(t, kind) })
	}
}

// requireCap skips the test when the backend cannot do what it is about to be
// asked to do.
func requireCap(t *testing.T, w *Watcher, c Capability, what string) {
	t.Helper()
	if !w.Capabilities().Has(c) {
		t.Skipf("backend %s cannot %s", w.Backend(), what)
	}
}

func TestConformanceCreateWriteRemove(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}

		file := filepath.Join(dir, "a.txt")

		writeFile(t, file, "hello")
		c.await(t, "create of a.txt", func(evs []Event) bool {
			return hasEvent(evs, file, Create)
		})

		writeFile(t, file, "hello world")
		c.await(t, "write to a.txt", func(evs []Event) bool {
			return hasEvent(evs, file, Write)
		})

		if err := os.Remove(file); err != nil {
			t.Fatalf("Remove: %s", err)
		}
		c.await(t, "removal of a.txt", func(evs []Event) bool {
			return hasEvent(evs, file, Remove)
		})

		if errs := c.errorsSeen(); len(errs) != 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
	})
}

func TestConformanceNotRecursiveByDefault(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}

		nested := filepath.Join(sub, "deep.txt")
		writeFile(t, nested, "content")

		c.refute(t, "event for a file in an unwatched subdirectory", func(evs []Event) bool {
			return hasEvent(evs, nested, Create)
		})
	})
}

func TestConformanceRecursiveViaSuffix(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		requireCap(t, w, CapRecursive, "watch recursively")

		if err := w.Add(dir + string(filepath.Separator) + "..."); err != nil {
			t.Fatalf("Add: %s", err)
		}

		nested := filepath.Join(sub, "deep.txt")
		writeFile(t, nested, "content")
		c.await(t, "create in a subdirectory", func(evs []Event) bool {
			return hasEvent(evs, nested, Create)
		})

		// A directory created after the watch was established must also be
		// covered. This is the case naive recursive implementations miss:
		// there is a window between the directory appearing and a watch being
		// placed on it, and anything created in that window is invisible
		// unless the new directory is rescanned after it is watched.
		later := filepath.Join(dir, "later")
		if err := os.MkdirAll(filepath.Join(later, "deeper"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}
		deepest := filepath.Join(later, "deeper", "x.txt")
		writeFile(t, deepest, "content")
		c.await(t, "create in a subdirectory made after the watch", func(evs []Event) bool {
			return hasEvent(evs, deepest, Create)
		})
	})
}

func TestConformanceRecursiveViaOption(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)
		requireCap(t, w, CapRecursive, "watch recursively")

		if err := w.AddWith(dir, WithRecursive()); err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		nested := filepath.Join(dir, "a", "b")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}
		file := filepath.Join(nested, "c.txt")
		writeFile(t, file, "content")

		c.await(t, "create deep in the tree", func(evs []Event) bool {
			return hasEvent(evs, file, Create)
		})
	})
}

func TestConformanceWatchSingleFile(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		file := filepath.Join(dir, "watched.txt")
		writeFile(t, file, "initial")

		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(file); err != nil {
			t.Fatalf("Add: %s", err)
		}

		writeFile(t, file, "changed and longer")
		c.await(t, "write to the watched file", func(evs []Event) bool {
			return hasEvent(evs, file, Write)
		})
	})
}

func TestConformanceExistingFilesAreNotCreations(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		existing := filepath.Join(dir, "already-here.txt")
		writeFile(t, existing, "content")

		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}

		c.refute(t, "create event for a pre-existing file", func(evs []Event) bool {
			return hasEvent(evs, existing, Create)
		})
	})
}

func TestConformanceRenameIsPaired(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		src := filepath.Join(dir, "before.txt")
		dst := filepath.Join(dir, "after.txt")
		writeFile(t, src, "content")

		w, c := newTestWatcherOn(t, kind)
		requireCap(t, w, CapPreciseRename, "distinguish a rename from a delete")

		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}
		if err := os.Rename(src, dst); err != nil {
			t.Fatalf("Rename: %s", err)
		}

		c.await(t, "rename of the old name and creation of the new", func(evs []Event) bool {
			return hasEvent(evs, src, Rename) && hasEvent(evs, dst, Create)
		})

		// The old name was moved, not deleted. Reporting Remove as well would
		// make a caller tear down state it should have carried over.
		if evs := c.snapshot(); hasEvent(evs, src, Remove) {
			t.Errorf("a rename reported REMOVE for the old name:\n%s", formatEvents(evs))
		}
	})
}

func TestConformanceWithOpsFilters(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(dir, WithOps(Create)); err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		file := filepath.Join(dir, "a.txt")
		writeFile(t, file, "hello")
		c.await(t, "create", func(evs []Event) bool { return hasEvent(evs, file, Create) })

		writeFile(t, file, "hello world, at greater length")
		c.refute(t, "write event on a watch restricted to creates", func(evs []Event) bool {
			return hasEvent(evs, file, Write)
		})
	})
}

func TestConformanceUnportableOps(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		file := filepath.Join(dir, "a.txt")
		writeFile(t, file, "content")

		w, c := newTestWatcherOn(t, kind)

		err := w.AddWith(dir, WithOps(UnportableCloseWrite))
		if !w.Capabilities().Has(CapUnportableOps) {
			// A backend that cannot report these must refuse the request. The
			// alternative — accepting it and never firing — is the worst
			// possible outcome, because it looks like "nothing happened".
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("AddWith(UnportableCloseWrite) on a backend without support = %v, want ErrUnsupported", err)
			}
			t.Skipf("backend %s cannot report unportable operations", kind)
		}
		if err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		// Write and close: the event fires on close, not on the write itself,
		// which is what makes it more useful than Write for "the file is
		// finished" detection.
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatalf("OpenFile: %s", err)
		}
		if _, err := f.WriteString("more"); err != nil {
			t.Fatalf("Write: %s", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %s", err)
		}

		c.await(t, "close-write", func(evs []Event) bool {
			return hasEvent(evs, file, UnportableCloseWrite)
		})
	})
}

func TestConformanceUnportableOpsNotDeliveredByDefault(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		file := filepath.Join(dir, "a.txt")
		writeFile(t, file, "content")

		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}

		f, err := os.Open(file)
		if err != nil {
			t.Fatalf("Open: %s", err)
		}
		_, _ = f.Read(make([]byte, 4))
		_ = f.Close()

		c.refute(t, "unportable event on a default watch", func(evs []Event) bool {
			for _, ev := range evs {
				if ev.Has(UnportableOpen | UnportableRead | UnportableCloseWrite | UnportableCloseRead) {
					return true
				}
			}
			return false
		})
	})
}

func TestConformanceWatchedPathRemoval(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "watched")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("RemoveAll: %s", err)
		}

		c.await(t, "removal of the watched directory itself", func(evs []Event) bool {
			return hasEvent(evs, dir, Remove)
		})

		// The watch died with the directory, so it must not linger in the
		// list. A stale entry would make WatchList a record of intent rather
		// than of what is actually being watched.
		c.await(t, "the watch to be dropped", func([]Event) bool {
			return len(w.WatchList()) == 0
		})
	})
}

func TestConformanceRemovedWatchStopsReporting(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}
		if err := w.Remove(dir); err != nil {
			t.Fatalf("Remove: %s", err)
		}

		file := filepath.Join(dir, "after-removal.txt")
		writeFile(t, file, "content")
		c.refute(t, "event from a removed watch", func(evs []Event) bool {
			return hasEvent(evs, file, Create)
		})
	})
}

func TestConformanceWatchList(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dirA, dirB := t.TempDir(), t.TempDir()
		w, _ := newTestWatcherOn(t, kind)

		if got := w.WatchList(); len(got) != 0 {
			t.Errorf("WatchList() on a fresh watcher = %v, want empty", got)
		}
		for _, d := range []string{dirA, dirB} {
			if err := w.Add(d); err != nil {
				t.Fatalf("Add(%s): %s", d, err)
			}
		}
		if got := w.WatchList(); len(got) != 2 {
			t.Errorf("WatchList() = %v, want 2 entries", got)
		}

		if err := w.Remove(dirA); err != nil {
			t.Fatalf("Remove: %s", err)
		}
		got := w.WatchList()
		if len(got) != 1 || got[0] != dirB {
			t.Errorf("WatchList() after Remove = %v, want [%s]", got, dirB)
		}
	})
}

func TestConformanceRemoveNonExistentWatch(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		w, _ := newTestWatcherOn(t, kind)
		if err := w.Remove(t.TempDir()); !errors.Is(err, ErrNonExistentWatch) {
			t.Fatalf("Remove(unwatched) = %v, want ErrNonExistentWatch", err)
		}
	})
}

func TestConformanceAddNonExistentPath(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		w, _ := newTestWatcherOn(t, kind)
		err := w.Add(filepath.Join(t.TempDir(), "does-not-exist"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Add(missing path) = %v, want it to wrap os.ErrNotExist", err)
		}
	})
}

func TestConformanceAddIsIdempotent(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)

		for range 3 {
			if err := w.Add(dir); err != nil {
				t.Fatalf("Add: %s", err)
			}
		}
		if got := w.WatchList(); len(got) != 1 {
			t.Fatalf("WatchList() after adding the same path 3 times = %v, want 1 entry", got)
		}

		// Re-adding must not duplicate delivery either.
		file := filepath.Join(dir, "a.txt")
		writeFile(t, file, "content")
		evs := c.await(t, "create", func(evs []Event) bool {
			return hasEvent(evs, file, Create)
		})

		var creates int
		for _, ev := range evs {
			if ev.Name == file && ev.Has(Create) {
				creates++
			}
		}
		if creates != 1 {
			t.Errorf("got %d create events for one file, want 1:\n%s", creates, formatEvents(evs))
		}
	})
}

func TestConformanceCloseWithNoConsumer(t *testing.T) {
	eachBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, err := NewWatcherWith(WithBackend(kind), WithPollInterval(testPollInterval))
		if err != nil {
			t.Fatalf("NewWatcherWith: %s", err)
		}
		if err := w.Add(dir); err != nil {
			t.Fatalf("Add: %s", err)
		}

		// Generate changes with nobody reading, parking the backend mid-send.
		for i := range 20 {
			writeFile(t, filepath.Join(dir, string(rune('a'+i))+".txt"), "content")
		}

		done := make(chan error, 1)
		go func() { done <- w.Close() }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Close: %s", err)
			}
		case <-timeAfterTestTimeout():
			t.Fatal("Close blocked with no consumer draining Events")
		}
	})
}

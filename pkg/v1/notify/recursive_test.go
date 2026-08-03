package notify

import (
	"os"
	"path/filepath"
	"testing"
)

// Recursion is where a watcher is most likely to be quietly wrong. The tests
// here target the cases that a straightforward implementation gets wrong in
// ways that never surface as an error: watches that were never placed, events
// that were never sent, and watches that were never released.

// eachRecursiveBackend runs fn against every backend that can watch
// recursively, which after wrapping is all of them.
func eachRecursiveBackend(t *testing.T, fn func(t *testing.T, kind Backend)) {
	t.Helper()
	eachBackend(t, func(t *testing.T, kind Backend) {
		w, _ := newTestWatcherOn(t, kind)
		if !w.Capabilities().Has(CapRecursive) {
			t.Skipf("backend %s cannot watch recursively", kind)
		}
		fn(t, kind)
	})
}

// TestRecursiveAdoptsDirectoryMovedIn is the case a naive implementation
// always misses.
//
// Renaming a populated directory into a watched tree is atomic: the kernel
// reports that one directory appeared, and says nothing about the hundred
// files that came with it. Those files existed before any watch could be
// placed on their parent, so they will never be reported by the kernel at all.
// Unless the new directory is walked on adoption, an entire subtree silently
// goes unwatched — and the caller has no way to know.
func TestRecursiveAdoptsDirectoryMovedIn(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		root := t.TempDir()
		watched := filepath.Join(root, "watched")
		staging := filepath.Join(root, "staging")

		// Build a populated tree outside the watched area.
		deep := filepath.Join(staging, "sub", "deeper")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}
		staged := filepath.Join(deep, "payload.txt")
		writeFile(t, staged, "content")

		if err := os.Mkdir(watched, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(watched, WithRecursive()); err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		moved := filepath.Join(watched, "moved")
		if err := os.Rename(staging, moved); err != nil {
			t.Fatalf("Rename: %s", err)
		}

		movedPayload := filepath.Join(moved, "sub", "deeper", "payload.txt")
		c.await(t, "creations for the whole moved-in subtree", func(evs []Event) bool {
			return hasEvent(evs, moved, Create) &&
				hasEvent(evs, filepath.Join(moved, "sub"), Create) &&
				hasEvent(evs, filepath.Join(moved, "sub", "deeper"), Create) &&
				hasEvent(evs, movedPayload, Create)
		})

		// Reporting the files is only half of it. The directories must
		// actually be watched now, or the tree is a snapshot rather than a
		// watch.
		writeFile(t, movedPayload, "content, now considerably longer")
		c.await(t, "a write inside the adopted subtree", func(evs []Event) bool {
			return hasEvent(evs, movedPayload, Write)
		})

		newFile := filepath.Join(moved, "sub", "deeper", "added-later.txt")
		writeFile(t, newFile, "content")
		c.await(t, "a create inside the adopted subtree", func(evs []Event) bool {
			return hasEvent(evs, newFile, Create)
		})
	})
}

// TestRecursiveNoDuplicateCreates guards the cost of fixing the adoption race.
//
// Walking a newly adopted directory can find a file that the kernel is also
// about to report, so the same creation could arrive twice. Duplicates are not
// harmless: a caller that reacts to Create by starting work would start it
// twice.
func TestRecursiveNoDuplicateCreates(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(dir, WithRecursive()); err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		// Create the directory and populate it immediately, so the walk and
		// the kernel are racing over the same files.
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}
		var files []string
		for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
			path := filepath.Join(sub, name)
			writeFile(t, path, "content")
			files = append(files, path)
		}

		evs := c.await(t, "creations for every file", func(evs []Event) bool {
			for _, f := range files {
				if !hasEvent(evs, f, Create) {
					return false
				}
			}
			return true
		})

		// Give any duplicate a chance to arrive before counting.
		c.refute(t, "a duplicate create", func(evs []Event) bool {
			for _, f := range files {
				if countEvents(evs, f, Create) > 1 {
					return true
				}
			}
			return false
		})

		for _, f := range files {
			if n := countEvents(evs, f, Create); n < 1 {
				t.Errorf("got %d create events for %s, want at least 1", n, f)
			}
		}
	})
}

// TestRecursivePrunesRemovedSubtree checks that watches are released when the
// directories they cover are deleted.
//
// A recursive watcher that adopts directories but never prunes them leaks a
// kernel watch per directory ever seen. On a tree with churn — a build
// directory, a cache — that exhausts the per-user watch limit and breaks every
// watcher in the session, not just this one.
func TestRecursivePrunesRemovedSubtree(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(dir, WithRecursive()); err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		rb, wrapped := w.state.backend.(*recursiveBackend)
		if !wrapped {
			t.Skipf("backend %s watches recursively without the wrapper", kind)
		}
		baseline := len(rb.inner.WatchList())

		// Create a subtree and wait for it to be adopted.
		sub := filepath.Join(dir, "sub")
		nested := filepath.Join(sub, "a", "b")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}
		marker := filepath.Join(nested, "marker.txt")
		writeFile(t, marker, "content")
		c.await(t, "the new subtree to be watched", func(evs []Event) bool {
			return hasEvent(evs, marker, Create)
		})

		if grown := len(rb.inner.WatchList()); grown <= baseline {
			t.Fatalf("inner watches did not grow when a subtree appeared: %d then %d", baseline, grown)
		}

		if err := os.RemoveAll(sub); err != nil {
			t.Fatalf("RemoveAll: %s", err)
		}

		c.await(t, "inner watches to be released", func([]Event) bool {
			return len(rb.inner.WatchList()) <= baseline
		})
	})
}

// TestRecursiveWatchListReportsRootsOnly checks that the internal machinery
// does not leak into the caller's view. A recursive watch is one thing the
// caller asked for, whatever it costs underneath.
func TestRecursiveWatchListReportsRootsOnly(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}

		w, _ := newTestWatcherOn(t, kind)
		if err := w.AddWith(dir, WithRecursive()); err != nil {
			t.Fatalf("AddWith: %s", err)
		}

		got := w.WatchList()
		if len(got) != 1 || got[0] != dir {
			t.Errorf("WatchList() = %v, want exactly [%s]", got, dir)
		}
	})
}

// TestRecursiveRemoveReleasesWholeTree checks that removing a recursive watch
// releases every watch placed to support it, not merely the root.
func TestRecursiveRemoveReleasesWholeTree(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(dir, WithRecursive()); err != nil {
			t.Fatalf("AddWith: %s", err)
		}
		if err := w.Remove(dir); err != nil {
			t.Fatalf("Remove: %s", err)
		}

		if got := w.WatchList(); len(got) != 0 {
			t.Errorf("WatchList() after Remove = %v, want empty", got)
		}
		if rb, wrapped := w.state.backend.(*recursiveBackend); wrapped {
			if got := rb.inner.WatchList(); len(got) != 0 {
				t.Errorf("inner watches after Remove = %v, want empty", got)
			}
		}

		deep := filepath.Join(dir, "a", "b", "c", "after.txt")
		writeFile(t, deep, "content")
		c.refute(t, "an event after the recursive watch was removed", func(evs []Event) bool {
			return hasEvent(evs, deep, Create)
		})
	})
}

// TestRecursiveNestedRootsPreferMostSpecific checks that overlapping recursive
// watches attribute an event to the innermost one covering it.
func TestRecursiveNestedRootsPreferMostSpecific(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		outer := t.TempDir()
		inner := filepath.Join(outer, "inner")
		if err := os.Mkdir(inner, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(outer, WithRecursive()); err != nil {
			t.Fatalf("AddWith(outer): %s", err)
		}
		if err := w.AddWith(inner, WithRecursive()); err != nil {
			t.Fatalf("AddWith(inner): %s", err)
		}

		file := filepath.Join(inner, "x.txt")
		writeFile(t, file, "content")

		evs := c.await(t, "create inside the nested watch", func(evs []Event) bool {
			return hasEvent(evs, file, Create)
		})
		// Overlapping watches must not multiply delivery.
		if n := countEvents(evs, file, Create); n != 1 {
			t.Errorf("got %d create events under overlapping watches, want 1:\n%s",
				n, formatEvents(evs))
		}
	})
}

// TestUnderOrEqual checks the path containment test used to attribute events
// to a watch. Comparing strings rather than path components would place
// "/a/bc" inside "/a/b".
func TestUnderOrEqual(t *testing.T) {
	sep := string(filepath.Separator)
	join := func(parts ...string) string { return filepath.Join(parts...) }

	tests := []struct {
		parent, path string
		want         bool
	}{
		{join("a", "b"), join("a", "b"), true},
		{join("a", "b"), join("a", "b", "c"), true},
		{join("a", "b"), join("a", "b", "c", "d"), true},
		{join("a", "b"), join("a", "bc"), false},
		{join("a", "b"), join("a", "bc", "d"), false},
		{join("a", "b"), join("a"), false},
		{join("a", "b"), join("x", "y"), false},
		{sep, join(sep, "a"), true},
	}
	for _, tt := range tests {
		if got := underOrEqual(tt.parent, tt.path); got != tt.want {
			t.Errorf("underOrEqual(%q, %q) = %v, want %v", tt.parent, tt.path, got, tt.want)
		}
	}
}

func countEvents(events []Event, path string, op Op) int {
	var n int
	for _, ev := range events {
		if ev.Name == path && ev.Has(op) {
			n++
		}
	}
	return n
}

// TestNestedRootsSurviveRemovingTheOuterOne is the regression test for a
// defect that produced no error at all.
//
// Two recursive watches can cover the same directory. If the inner watch that
// covers it is torn down when either one is removed, the survivor goes quiet:
// it is still listed, still reports no error, and simply stops seeing part of
// its tree. Silent partial coverage is the worst failure a watcher has,
// because nothing distinguishes it from a quiet filesystem.
func TestNestedRootsSurviveRemovingTheOuterOne(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		outer := t.TempDir()
		inner := filepath.Join(outer, "inner")
		if err := os.Mkdir(inner, 0o755); err != nil {
			t.Fatalf("Mkdir: %s", err)
		}

		w, c := newTestWatcherOn(t, kind)
		if err := w.AddWith(outer, WithRecursive()); err != nil {
			t.Fatalf("AddWith(outer): %s", err)
		}
		if err := w.AddWith(inner, WithRecursive()); err != nil {
			t.Fatalf("AddWith(inner): %s", err)
		}

		// Removing the outer watch must not disturb the inner one, even though
		// both were covering the same directory.
		if err := w.Remove(outer); err != nil {
			t.Fatalf("Remove(outer): %s", err)
		}

		file := filepath.Join(inner, "still-watched.txt")
		writeFile(t, file, "content")
		c.await(t, "an event from the surviving nested watch", func(evs []Event) bool {
			return hasEvent(evs, file, Create)
		})
	})
}

// TestOverlappingRootsReleaseEverythingEventually checks the other direction:
// shared watches must not outlive the last watch that needed them, or a
// long-running process leaks a kernel watch per directory ever shared.
func TestOverlappingRootsReleaseEverythingEventually(t *testing.T) {
	eachRecursiveBackend(t, func(t *testing.T, kind Backend) {
		outer := t.TempDir()
		inner := filepath.Join(outer, "inner")
		if err := os.MkdirAll(filepath.Join(inner, "deep"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %s", err)
		}

		w, _ := newTestWatcherOn(t, kind)
		rb, wrapped := w.state.backend.(*recursiveBackend)
		if !wrapped {
			t.Skipf("backend %s watches recursively without the wrapper", kind)
		}

		for _, root := range []string{outer, inner} {
			if err := w.AddWith(root, WithRecursive()); err != nil {
				t.Fatalf("AddWith(%s): %s", root, err)
			}
		}
		for _, root := range []string{outer, inner} {
			if err := w.Remove(root); err != nil {
				t.Fatalf("Remove(%s): %s", root, err)
			}
		}

		if got := rb.inner.WatchList(); len(got) != 0 {
			t.Errorf("inner watches after removing both roots = %v, want none", got)
		}
		rb.mu.Lock()
		refs := len(rb.dirRefs)
		rb.mu.Unlock()
		if refs != 0 {
			t.Errorf("%d reference counts left behind, want 0", refs)
		}
	})
}

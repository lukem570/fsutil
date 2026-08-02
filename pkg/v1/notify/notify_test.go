package notify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPollInterval keeps the polling backend responsive enough that tests
// finish quickly, without making them so tight that a loaded CI machine
// produces spurious failures.
const testPollInterval = 15 * time.Millisecond

// testTimeout bounds how long a test waits for an expected event. It is
// generous relative to the poll interval: the failure we care about is "the
// event never arrives", not "the event was slow".
const testTimeout = 5 * time.Second

// collector drains a watcher's channels into slices.
//
// Draining is not optional. Both channels are unbuffered by default, so a test
// that does not read them blocks the backend and then deadlocks its own
// Close.
type collector struct {
	mu     sync.Mutex
	events []Event
	errs   []error
	closed chan struct{}
}

func collect(t *testing.T, w *Watcher) *collector {
	t.Helper()

	c := &collector{closed: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for ev := range w.Events {
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for err := range w.Errors {
			c.mu.Lock()
			c.errs = append(c.errs, err)
			c.mu.Unlock()
		}
	}()
	go func() {
		wg.Wait()
		close(c.closed)
	}()

	return c
}

// snapshot returns the events seen so far.
func (c *collector) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

func (c *collector) errorsSeen() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errs...)
}

// await blocks until pred is satisfied by the events seen so far, and fails
// the test if that has not happened before the timeout.
func (c *collector) await(t *testing.T, what string, pred func([]Event) bool) []Event {
	t.Helper()

	deadline := time.Now().Add(testTimeout)
	for {
		events := c.snapshot()
		if pred(events) {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s\nevents seen (%d):\n%s",
				what, len(events), formatEvents(events))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// refute fails if pred becomes true within a short grace period. It is for
// asserting that an event is *not* delivered, which cannot be proven, only
// given a fair chance to fail.
func (c *collector) refute(t *testing.T, what string, pred func([]Event) bool) {
	t.Helper()

	deadline := time.Now().Add(20 * testPollInterval)
	for time.Now().Before(deadline) {
		if events := c.snapshot(); pred(events) {
			t.Fatalf("unexpected %s\nevents seen (%d):\n%s",
				what, len(events), formatEvents(events))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func formatEvents(events []Event) string {
	if len(events) == 0 {
		return "  (none)"
	}
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("  " + ev.String() + "\n")
	}
	return b.String()
}

// hasEvent reports whether any event names path and includes op.
func hasEvent(events []Event, path string, op Op) bool {
	for _, ev := range events {
		if ev.Name == path && ev.Has(op) {
			return true
		}
	}
	return false
}

// newTestWatcher creates a polling watcher wired for fast, deterministic tests
// and closes it when the test ends.
func newTestWatcher(t *testing.T, opts ...Option) (*Watcher, *collector) {
	t.Helper()

	opts = append([]Option{
		WithBackend(BackendPoll),
		WithPollInterval(testPollInterval),
		WithEventBuffer(256),
	}, opts...)

	w, err := NewWatcherWith(opts...)
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	c := collect(t, w)
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close: %s", err)
		}
		select {
		case <-c.closed:
		case <-time.After(testTimeout):
			t.Error("Close returned but the event channels were never closed")
		}
	})
	return w, c
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileBytes(path, []byte(content)); err != nil {
		t.Fatalf("writing %s: %s", path, err)
	}
}

func writeFileBytes(path string, content []byte) error {
	return os.WriteFile(path, content, 0o644)
}

// ---------------------------------------------------------------- unit tests

func TestOpString(t *testing.T) {
	tests := []struct {
		op   Op
		want string
	}{
		{0, "[no events]"},
		{Create, "CREATE"},
		{Create | Write, "CREATE|WRITE"},
		{Remove | Rename | Chmod, "REMOVE|RENAME|CHMOD"},
		{UnportableCloseWrite, "CLOSE_WRITE"},
		{Create | 1<<30, "CREATE|0x40000000"},
		{1 << 30, "0x40000000"},
	}
	for _, tt := range tests {
		if got := tt.op.String(); got != tt.want {
			t.Errorf("Op(%#b).String() = %q, want %q", uint32(tt.op), got, tt.want)
		}
	}
}

func TestOpHas(t *testing.T) {
	op := Create | Write
	for _, want := range []Op{Create, Write, Create | Write} {
		if !op.Has(want) {
			t.Errorf("(%s).Has(%s) = false, want true", op, want)
		}
	}
	if op.Has(Remove) {
		t.Errorf("(%s).Has(REMOVE) = true, want false", op)
	}
}

func TestEventString(t *testing.T) {
	ev := Event{Name: "/tmp/x", Op: Create}
	if got, want := ev.String(), `CREATE        "/tmp/x"`; got != want {
		t.Errorf("Event.String() = %q, want %q", got, want)
	}
}

func TestSplitRecursive(t *testing.T) {
	tests := []struct {
		in        string
		wantPath  string
		wantRecur bool
	}{
		{"/tmp/x", filepath.Clean("/tmp/x"), false},
		{"/tmp/x/...", filepath.Clean("/tmp/x"), true},
		{"...", ".", true},
		{"relative/...", "relative", true},
		{"/tmp/x/", filepath.Clean("/tmp/x"), false},
		// "..." only means recursion as a trailing component.
		{"/tmp/.../x", filepath.Clean("/tmp/.../x"), false},
	}
	for _, tt := range tests {
		gotPath, gotRecur := splitRecursive(tt.in)
		if gotPath != tt.wantPath || gotRecur != tt.wantRecur {
			t.Errorf("splitRecursive(%q) = (%q, %v), want (%q, %v)",
				tt.in, gotPath, gotRecur, tt.wantPath, tt.wantRecur)
		}
	}
}

func TestCapabilityHas(t *testing.T) {
	c := CapRecursive | CapNoFollow
	if !c.Has(CapRecursive) {
		t.Error("Has(CapRecursive) = false, want true")
	}
	// Has requires *all* the requested bits, not any of them.
	if c.Has(CapRecursive | CapPrivileged) {
		t.Error("Has(CapRecursive|CapPrivileged) = true, want false")
	}
}

func TestBackendsAvailable(t *testing.T) {
	got := Backends()
	if len(got) == 0 {
		t.Fatal("Backends() is empty; polling should be available everywhere")
	}
	var hasPoll bool
	for _, b := range got {
		if b == BackendPoll {
			hasPoll = true
		}
	}
	if !hasPoll {
		t.Errorf("Backends() = %v, want it to include %s", got, BackendPoll)
	}
}

func TestUnsupportedBackendIsRejected(t *testing.T) {
	// A backend that exists as a constant but is not compiled in here must be
	// refused outright rather than quietly substituted.
	_, err := NewWatcherWith(WithBackend(Backend(200)))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewWatcherWith(unknown backend) error = %v, want ErrUnsupported", err)
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	if _, err := NewWatcherWith(WithPollInterval(0)); err == nil {
		t.Error("NewWatcherWith(WithPollInterval(0)) succeeded, want an error")
	}
	if _, err := NewWatcherWith(WithFDBudget(-1)); err == nil {
		t.Error("NewWatcherWith(WithFDBudget(-1)) succeeded, want an error")
	}
}

// ------------------------------------------------------------ watcher tests

func TestWatchDirectoryReportsCreateWriteRemove(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWatcher(t)
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
}

func TestWatchDirectoryIsNotRecursiveByDefault(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %s", err)
	}

	w, c := newTestWatcher(t)
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}

	nested := filepath.Join(sub, "deep.txt")
	writeFile(t, nested, "content")

	c.refute(t, "event for a file in an unwatched subdirectory", func(evs []Event) bool {
		return hasEvent(evs, nested, Create)
	})
}

func TestWatchRecursiveViaSuffix(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %s", err)
	}

	w, c := newTestWatcher(t)
	if err := w.Add(dir + string(filepath.Separator) + "..."); err != nil {
		t.Fatalf("Add: %s", err)
	}

	nested := filepath.Join(sub, "deep.txt")
	writeFile(t, nested, "content")
	c.await(t, "create in a subdirectory", func(evs []Event) bool {
		return hasEvent(evs, nested, Create)
	})

	// A directory created after the watch was established must also be
	// covered, which is the part naive recursive implementations miss.
	later := filepath.Join(dir, "later")
	if err := os.MkdirAll(filepath.Join(later, "deeper"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %s", err)
	}
	deepest := filepath.Join(later, "deeper", "x.txt")
	writeFile(t, deepest, "content")
	c.await(t, "create in a subdirectory made after the watch", func(evs []Event) bool {
		return hasEvent(evs, deepest, Create)
	})
}

func TestWatchRecursiveViaOption(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWatcher(t)
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
}

func TestWatchSingleFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "watched.txt")
	writeFile(t, file, "initial")

	w, c := newTestWatcher(t)
	if err := w.Add(file); err != nil {
		t.Fatalf("Add: %s", err)
	}

	writeFile(t, file, "changed and longer")
	c.await(t, "write to the watched file", func(evs []Event) bool {
		return hasEvent(evs, file, Write)
	})
}

func TestExistingFilesAreNotReportedAsCreated(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "already-here.txt")
	writeFile(t, existing, "content")

	w, c := newTestWatcher(t)
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}

	c.refute(t, "create event for a pre-existing file", func(evs []Event) bool {
		return hasEvent(evs, existing, Create)
	})
}

func TestRenameIsPaired(t *testing.T) {
	if !fileIDSupported {
		t.Skip("rename pairing needs stable file identity, which this platform lacks")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "before.txt")
	dst := filepath.Join(dir, "after.txt")
	writeFile(t, src, "content")

	w, c := newTestWatcher(t)
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}

	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("Rename: %s", err)
	}

	c.await(t, "rename of the old name", func(evs []Event) bool {
		return hasEvent(evs, src, Rename) && hasEvent(evs, dst, Create)
	})

	// The old name was moved, not deleted; reporting Remove as well would be
	// wrong and would make callers delete state they should have moved.
	if evs := c.snapshot(); hasEvent(evs, src, Remove) {
		t.Errorf("a rename reported REMOVE for the old name:\n%s", formatEvents(evs))
	}
}

func TestWithOpsFiltersEvents(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWatcher(t)
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
}

func TestWithOpsRejectsEmptySet(t *testing.T) {
	w, _ := newTestWatcher(t)
	err := w.AddWith(t.TempDir(), WithOps())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AddWith(WithOps()) error = %v, want ErrUnsupported", err)
	}
}

func TestUnportableOpsRejectedWhenUnsupported(t *testing.T) {
	// Polling cannot observe opens or reads: they leave no trace in the
	// metadata it compares. Asking for them must fail loudly rather than
	// silently never firing.
	w, _ := newTestWatcher(t)
	err := w.AddWith(t.TempDir(), WithOps(UnportableCloseWrite))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AddWith(WithOps(UnportableCloseWrite)) error = %v, want ErrUnsupported", err)
	}
}

func TestWatchList(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	w, _ := newTestWatcher(t)

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
}

func TestRemoveNonExistentWatch(t *testing.T) {
	w, _ := newTestWatcher(t)
	err := w.Remove(t.TempDir())
	if !errors.Is(err, ErrNonExistentWatch) {
		t.Fatalf("Remove(unwatched) error = %v, want ErrNonExistentWatch", err)
	}
}

func TestRemovedWatchStopsReporting(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWatcher(t)
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
}

func TestAddNonExistentPath(t *testing.T) {
	w, _ := newTestWatcher(t)
	err := w.Add(filepath.Join(t.TempDir(), "does-not-exist"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Add(missing path) error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestWatchedPathRemovalIsReported(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "watched")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %s", err)
	}

	w, c := newTestWatcher(t)
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %s", err)
	}

	c.await(t, "removal of the watched directory itself", func(evs []Event) bool {
		return hasEvent(evs, dir, Remove)
	})

	// The watch is gone with the directory, so it must not linger in the list.
	c.await(t, "the watch to be dropped", func([]Event) bool {
		return len(w.WatchList()) == 0
	})
}

func TestCloseIsIdempotent(t *testing.T) {
	w, err := NewWatcherWith(WithBackend(BackendPoll), WithPollInterval(testPollInterval))
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	collect(t, w)

	for i := range 3 {
		if err := w.Close(); err != nil {
			t.Errorf("Close call %d: %s", i+1, err)
		}
	}
}

func TestOperationsAfterCloseReturnErrClosed(t *testing.T) {
	w, err := NewWatcherWith(WithBackend(BackendPoll), WithPollInterval(testPollInterval))
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	collect(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	dir := t.TempDir()
	if err := w.Add(dir); !errors.Is(err, ErrClosed) {
		t.Errorf("Add after Close = %v, want ErrClosed", err)
	}
	if err := w.Remove(dir); !errors.Is(err, ErrClosed) {
		t.Errorf("Remove after Close = %v, want ErrClosed", err)
	}
	if got := w.WatchList(); got != nil {
		t.Errorf("WatchList after Close = %v, want nil", got)
	}
}

// TestCloseWithNoConsumer is the deadlock regression test.
//
// Close must not depend on anyone reading the Events channel. If it does, any
// program that stops consuming before shutting down hangs forever, and that is
// the most natural way to write the shutdown path.
func TestCloseWithNoConsumer(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWatcherWith(WithBackend(BackendPoll), WithPollInterval(testPollInterval))
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add: %s", err)
	}

	// Generate changes with nobody reading, so the backend is parked mid-send.
	for i := range 20 {
		writeFile(t, filepath.Join(dir, string(rune('a'+i))+".txt"), "content")
	}
	time.Sleep(4 * testPollInterval)

	done := make(chan error, 1)
	go func() { done <- w.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %s", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Close blocked with no consumer draining Events")
	}
}

func TestBackendReportsItself(t *testing.T) {
	w, _ := newTestWatcher(t)
	if got := w.Backend(); got != BackendPoll {
		t.Errorf("Backend() = %s, want %s", got, BackendPoll)
	}
	if got := w.Backend(); got == BackendAuto {
		t.Error("Backend() returned BackendAuto; it must name a concrete backend")
	}
	if !w.Capabilities().Has(CapRecursive) {
		t.Error("the polling backend should report CapRecursive")
	}
}

func TestConcurrentAddRemove(t *testing.T) {
	w, _ := newTestWatcher(t)

	dirs := make([]string, 8)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	for _, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := w.Add(dir); err != nil {
					t.Errorf("Add(%s): %s", dir, err)
					return
				}
				if err := w.Remove(dir); err != nil && !errors.Is(err, ErrNonExistentWatch) {
					t.Errorf("Remove(%s): %s", dir, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

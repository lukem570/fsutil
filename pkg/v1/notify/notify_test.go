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
// produces spurious failures. Backends that hear from the kernel ignore it.
const testPollInterval = 15 * time.Millisecond

// testTimeout bounds how long a test waits for an expected event. It is
// generous relative to the poll interval: the failure worth reporting is "the
// event never arrived", not "the event was slow".
const testTimeout = 5 * time.Second

func timeAfterTestTimeout() <-chan time.Time { return time.After(testTimeout) }

// collector drains a watcher's channels into slices.
//
// Draining is not optional. The Errors channel is unbuffered, so a test that
// does not read it blocks the backend on its first error and then deadlocks
// its own Close.
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

// await blocks until pred is satisfied by the events seen so far, failing the
// test if that has not happened before the timeout.
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

// refute fails if pred becomes true within a grace period.
//
// The absence of an event cannot be proven, only given a fair chance to
// appear. The window is scaled to the poll interval so that the slowest
// backend still gets several opportunities to be wrong.
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

func hasEvent(events []Event, path string, op Op) bool {
	for _, ev := range events {
		if ev.Name == path && ev.Has(op) {
			return true
		}
	}
	return false
}

// newTestWatcherOn creates a watcher using a specific backend, wired for fast
// tests, and closes it when the test ends.
func newTestWatcherOn(t *testing.T, kind Backend, opts ...Option) (*Watcher, *collector) {
	t.Helper()

	opts = append([]Option{
		WithBackend(kind),
		WithPollInterval(testPollInterval),
		WithEventBuffer(256),
	}, opts...)

	w, err := NewWatcherWith(opts...)
	if err != nil {
		t.Fatalf("NewWatcherWith(%s): %s", kind, err)
	}
	c := collect(t, w)
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close: %s", err)
		}
		select {
		case <-c.closed:
		case <-timeAfterTestTimeout():
			t.Error("Close returned but the event channels were never closed")
		}
	})
	return w, c
}

// newTestWatcher creates a polling watcher, for tests whose subject is the
// package rather than any particular backend.
func newTestWatcher(t *testing.T, opts ...Option) (*Watcher, *collector) {
	t.Helper()
	return newTestWatcherOn(t, BackendPoll, opts...)
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
		// "..." means recursion only as a trailing component.
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
	// Has requires all the requested bits, not any of them.
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
	t.Logf("backends available on this host: %v", got)
}

func TestUnsupportedBackendIsRejected(t *testing.T) {
	// A backend that is not compiled in here must be refused outright rather
	// than quietly substituted with something else.
	_, err := NewWatcherWith(WithBackend(Backend(200)))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewWatcherWith(unknown backend) = %v, want ErrUnsupported", err)
	}
}

func TestAutoSelectionPrefersNative(t *testing.T) {
	w, err := NewWatcherWith()
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	defer w.Close()

	if w.Backend() == BackendAuto {
		t.Fatal("Backend() = auto; a concrete backend must have been chosen")
	}
	// Polling is the last resort, so on a host with a native backend it must
	// not be what automatic selection settles on.
	if available := Backends(); len(available) > 1 && w.Backend() == BackendPoll {
		t.Errorf("Backend() = %s despite %v being available", w.Backend(), available)
	}
	t.Logf("automatic selection chose %s", w.Backend())
}

func TestPrivilegedBackendsAreNotChosenAutomatically(t *testing.T) {
	w, err := NewWatcherWith()
	if err != nil {
		t.Fatalf("NewWatcherWith: %s", err)
	}
	defer w.Close()

	// A backend that needs elevation may be usable only because this process
	// happens to be privileged, which is not a property to depend on silently.
	if w.Capabilities().Has(CapPrivileged) {
		t.Errorf("automatic selection chose the privileged backend %s", w.Backend())
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

func TestWithOpsRejectsEmptySet(t *testing.T) {
	w, _ := newTestWatcher(t)
	if err := w.AddWith(t.TempDir(), WithOps()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AddWith(WithOps()) = %v, want ErrUnsupported", err)
	}
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

func TestBackendReportsItself(t *testing.T) {
	w, _ := newTestWatcher(t)
	if got := w.Backend(); got != BackendPoll {
		t.Errorf("Backend() = %s, want %s", got, BackendPoll)
	}
	if !w.Capabilities().Has(CapRecursive) {
		t.Error("the polling backend should report CapRecursive")
	}
}

func TestConcurrentAddRemove(t *testing.T) {
	for _, kind := range Backends() {
		t.Run(kind.String(), func(t *testing.T) {
			w, _ := newTestWatcherOn(t, kind)

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
		})
	}
}

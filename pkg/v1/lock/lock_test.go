package lock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// A lock that only ever coordinates goroutines is a mutex with extra steps.
// The property that matters is coordination between *processes*, and no
// single-process test can demonstrate it: the process-local registry would
// produce identical results whether or not the operating system was involved
// at all. So the tests below run a second copy of this binary and contend with
// it for real.

const (
	helperEnv     = "FSUTIL_LOCK_HELPER_PATH"
	helperHoldEnv = "FSUTIL_LOCK_HELPER_HOLD"
	helperModeEnv = "FSUTIL_LOCK_HELPER_MODE"
)

// TestMain doubles as the entry point for the helper process. When the
// environment names a lock file, this binary acts as a second participant
// rather than as a test.
func TestMain(m *testing.M) {
	if path := os.Getenv(helperEnv); path != "" {
		os.Exit(helperMain(path))
	}
	os.Exit(m.Run())
}

// helperMain takes the lock, announces that it has it, and holds on.
//
// The announcement is what makes the test deterministic: the parent waits to
// see it rather than sleeping and hoping, so a slow machine makes the test
// slower rather than flaky.
func helperMain(path string) int {
	shared := os.Getenv(helperModeEnv) == "shared"

	l, err := New(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: %s\n", err)
		return 1
	}
	defer l.Close()

	var ok bool
	if shared {
		ok, err = l.TryRLock()
	} else {
		ok, err = l.TryLock()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: %s\n", err)
		return 1
	}
	if !ok {
		fmt.Println("busy")
		return 2
	}

	fmt.Println("locked")
	os.Stdout.Sync()

	hold, _ := time.ParseDuration(os.Getenv(helperHoldEnv))
	if hold <= 0 {
		hold = time.Hour // held until killed
	}
	time.Sleep(hold)

	_ = l.Unlock()
	return 0
}

// helper starts a second process holding the lock, and returns it once it has
// confirmed that it does.
func startHelper(t *testing.T, path string, mode string, hold time.Duration) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		helperEnv+"="+path,
		helperModeEnv+"="+mode,
		helperHoldEnv+"="+hold.String(),
	)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %s", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %s", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	line := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			line <- scanner.Text()
		}
		close(line)
	}()

	select {
	case got, ok := <-line:
		if !ok {
			t.Fatal("helper exited without taking the lock")
		}
		if got != "locked" {
			t.Fatalf("helper said %q, want \"locked\"", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the helper to take the lock")
	}
	return cmd
}

func newLock(t *testing.T, opts ...Option) *Lock {
	t.Helper()
	return newLockAt(t, filepath.Join(t.TempDir(), "test.lock"), opts...)
}

func newLockAt(t *testing.T, path string, opts ...Option) *Lock {
	t.Helper()

	l, err := New(path, opts...)
	if err != nil {
		t.Fatalf("New(%s): %s", path, err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("Close: %s", err)
		}
	})
	return l
}

// ------------------------------------------------------- single process

func TestExclusiveExcludesExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a := newLockAt(t, path)
	b := newLockAt(t, path)

	ok, err := a.TryLock()
	if err != nil || !ok {
		t.Fatalf("TryLock on a free lock = (%v, %v), want (true, nil)", ok, err)
	}
	if a.Mode() != Exclusive {
		t.Errorf("Mode() = %s, want %s", a.Mode(), Exclusive)
	}

	ok, err = b.TryLock()
	if err != nil {
		t.Fatalf("TryLock on a held lock returned an error: %s", err)
	}
	if ok {
		t.Fatal("TryLock succeeded while the lock was held elsewhere")
	}

	if err := a.Unlock(); err != nil {
		t.Fatalf("Unlock: %s", err)
	}

	ok, err = b.TryLock()
	if err != nil || !ok {
		t.Fatalf("TryLock after release = (%v, %v), want (true, nil)", ok, err)
	}
	if err := b.Unlock(); err != nil {
		t.Fatalf("Unlock: %s", err)
	}
}

func TestSharedAllowsSharedButNotExclusive(t *testing.T) {
	if Mechanism() == "exclusive-create" {
		t.Skip("this platform grants shared locks as exclusive ones")
	}

	path := filepath.Join(t.TempDir(), "s.lock")
	a, b, c := newLockAt(t, path), newLockAt(t, path), newLockAt(t, path)

	for i, l := range []*Lock{a, b} {
		ok, err := l.TryRLock()
		if err != nil || !ok {
			t.Fatalf("TryRLock %d = (%v, %v), want (true, nil)", i, ok, err)
		}
	}

	ok, err := c.TryLock()
	if err != nil {
		t.Fatalf("TryLock alongside shared holders: %s", err)
	}
	if ok {
		t.Fatal("an exclusive lock was granted while shared locks were held")
	}

	if err := a.Unlock(); err != nil {
		t.Fatalf("Unlock: %s", err)
	}
	// One shared holder remains, so exclusive must still be refused.
	if ok, _ := c.TryLock(); ok {
		t.Fatal("an exclusive lock was granted while one shared holder remained")
	}

	if err := b.Unlock(); err != nil {
		t.Fatalf("Unlock: %s", err)
	}
	if ok, err := c.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock after all shared holders left = (%v, %v), want (true, nil)", ok, err)
	}
	_ = c.Unlock()
}

func TestDoubleAcquireIsRejected(t *testing.T) {
	l := newLock(t)
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}
	// Taking a lock this same object already holds would deadlock against
	// itself on some platforms and quietly succeed on others, so it is
	// reported instead.
	if _, err := l.TryLock(); err == nil {
		t.Error("re-acquiring an already-held lock succeeded, want an error")
	}
	_ = l.Unlock()
}

func TestUnlockWithoutHoldingIsAnError(t *testing.T) {
	l := newLock(t)
	if err := l.Unlock(); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Unlock without holding = %v, want ErrNotHeld", err)
	}
}

func TestOperationsAfterCloseFail(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "c.lock"))
	if err != nil {
		t.Fatalf("New: %s", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}
	// Close is idempotent.
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %s", err)
	}
	if _, err := l.TryLock(); !errors.Is(err, ErrClosed) {
		t.Errorf("TryLock after Close = %v, want ErrClosed", err)
	}
	if err := l.Unlock(); !errors.Is(err, ErrClosed) {
		t.Errorf("Unlock after Close = %v, want ErrClosed", err)
	}
}

func TestCloseReleasesAHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.lock")

	held, err := New(path)
	if err != nil {
		t.Fatalf("New: %s", err)
	}
	if ok, err := held.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	other := newLockAt(t, path)
	if ok, err := other.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock after the holder was closed = (%v, %v), want (true, nil)", ok, err)
	}
	_ = other.Unlock()
}

func TestLockWaitsAndThenSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.lock")
	holder := newLockAt(t, path)
	waiter := newLockAt(t, path)

	if ok, err := holder.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(released)
		_ = holder.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waiter.Lock(ctx); err != nil {
		t.Fatalf("Lock: %s", err)
	}
	select {
	case <-released:
	default:
		t.Error("Lock returned before the holder released it")
	}
	_ = waiter.Unlock()
}

func TestLockRespectsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.lock")
	holder := newLockAt(t, path)
	waiter := newLockAt(t, path)

	if ok, err := holder.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := waiter.Lock(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock on a cancelled context = %v, want context.Canceled", err)
	}
	// The point of retrying rather than blocking in the kernel is that
	// cancellation actually takes effect.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s to take effect", elapsed)
	}
	_ = holder.Unlock()
}

func TestWithTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeout.lock")
	holder := newLockAt(t, path)
	waiter := newLockAt(t, path, WithTimeout(50*time.Millisecond))

	if ok, err := holder.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}

	err := waiter.Lock(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock with an expired timeout = %v, want context.DeadlineExceeded", err)
	}
	_ = holder.Unlock()
}

// TestMutualExclusionBetweenGoroutines checks the property the lock exists for,
// rather than merely that acquisition returns true.
func TestMutualExclusionBetweenGoroutines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutex.lock")

	const goroutines = 8
	const iterations = 40

	var (
		inside  int
		overlap int
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()

			l, err := New(path)
			if err != nil {
				t.Errorf("New: %s", err)
				return
			}
			defer l.Close()

			for range iterations {
				if err := l.Lock(context.Background()); err != nil {
					t.Errorf("Lock: %s", err)
					return
				}

				mu.Lock()
				inside++
				if inside > 1 {
					overlap++
				}
				mu.Unlock()

				mu.Lock()
				inside--
				mu.Unlock()

				if err := l.Unlock(); err != nil {
					t.Errorf("Unlock: %s", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if overlap != 0 {
		t.Errorf("%d overlapping entries into the critical section", overlap)
	}
}

// TestSamePathDifferentNamesShareOneLock checks that identity, not spelling,
// decides what is being locked.
func TestSamePathDifferentNamesShareOneLock(t *testing.T) {
	dir := t.TempDir()
	direct := filepath.Join(dir, "target.lock")

	a := newLockAt(t, direct)
	if ok, err := a.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}

	// A relative-looking path that resolves to the same file.
	indirect := filepath.Join(dir, "sub", "..", "target.lock")
	b := newLockAt(t, indirect)
	if ok, err := b.TryLock(); err != nil {
		t.Fatalf("TryLock via another spelling: %s", err)
	} else if ok {
		t.Error("the same file was locked twice through two spellings of its path")
	}
	_ = a.Unlock()
}

func TestHolderInfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holder.lock")
	l := newLockAt(t, path, WithHolderInfo())

	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}
	defer l.Unlock()

	h, err := l.Holder()
	if err != nil {
		t.Fatalf("Holder: %s", err)
	}
	if h.PID != os.Getpid() {
		t.Errorf("Holder().PID = %d, want %d", h.PID, os.Getpid())
	}
	if h.Since.IsZero() {
		t.Error("Holder().Since is zero")
	}
}

func TestHolderInfoAbsentByDefault(t *testing.T) {
	l := newLock(t)
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}
	defer l.Unlock()

	// Recording costs a write per acquisition, so it is opt-in. Absent
	// information must read as absent rather than as an error.
	h, err := l.Holder()
	if err != nil {
		t.Fatalf("Holder: %s", err)
	}
	if h.PID != 0 {
		t.Errorf("Holder().PID = %d on a lock without WithHolderInfo, want 0", h.PID)
	}
}

func TestInvalidOptionsAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opts.lock")
	for _, tt := range []struct {
		name string
		opt  Option
	}{
		{"zero retry interval", WithRetryInterval(0)},
		{"negative timeout", WithTimeout(-time.Second)},
		{"max below initial", WithMaxRetryInterval(time.Nanosecond)},
	} {
		if _, err := New(path, tt.opt); err == nil {
			t.Errorf("New with %s succeeded, want an error", tt.name)
		}
	}
}

func TestCreateDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c.lock")
	if _, err := New(path); err == nil {
		t.Error("New in a missing directory succeeded without WithCreateDirs")
	}

	l := newLockAt(t, path, WithCreateDirs())
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}
	_ = l.Unlock()
}

// ------------------------------------------------------- multiple processes

func TestExclusionBetweenProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proc.lock")
	startHelper(t, path, "exclusive", time.Hour)

	l := newLockAt(t, path)
	ok, err := l.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %s", err)
	}
	if ok {
		t.Fatal("acquired a lock that another process holds")
	}
}

func TestSharedLocksAreSharedBetweenProcesses(t *testing.T) {
	if Mechanism() == "exclusive-create" {
		t.Skip("this platform grants shared locks as exclusive ones")
	}

	path := filepath.Join(t.TempDir(), "shared-proc.lock")
	startHelper(t, path, "shared", time.Hour)

	l := newLockAt(t, path)

	if ok, err := l.TryRLock(); err != nil || !ok {
		t.Fatalf("TryRLock alongside another process's shared lock = (%v, %v), want (true, nil)", ok, err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("Unlock: %s", err)
	}

	if ok, err := l.TryLock(); err != nil {
		t.Fatalf("TryLock: %s", err)
	} else if ok {
		t.Fatal("acquired an exclusive lock while another process held it shared")
	}
}

// TestLockSurvivesHolderExit checks that the operating system cleans up after
// a process that dies while holding a lock.
//
// This is the property that makes file locks usable for anything important. A
// lock that outlived its holder would need a human to clear it after every
// crash, which in practice means it gets cleared by a script that guesses, and
// then it is not a lock any more.
func TestLockReleasedWhenHolderIsKilled(t *testing.T) {
	if Mechanism() == "exclusive-create" {
		t.Skip("this platform cannot release a lock whose holder died")
	}

	path := filepath.Join(t.TempDir(), "killed.lock")
	cmd := startHelper(t, path, "exclusive", time.Hour)

	l := newLockAt(t, path)
	if ok, _ := l.TryLock(); ok {
		t.Fatal("acquired a lock the helper holds")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the helper: %s", err)
	}
	_, _ = cmd.Process.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := l.Lock(ctx); err != nil {
		t.Fatalf("acquiring after the holder was killed: %s", err)
	}
	_ = l.Unlock()
}

func TestWaitsForAnotherProcessToRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handover.lock")
	startHelper(t, path, "exclusive", 250*time.Millisecond)

	l := newLockAt(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	if err := l.Lock(ctx); err != nil {
		t.Fatalf("Lock: %s", err)
	}
	defer l.Unlock()

	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("Lock returned after %s, before the other process could have released", elapsed)
	}
}

// ------------------------------------------------------- abandoned locks

//go:noinline
func abandonLock(t *testing.T, path string) {
	t.Helper()

	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %s", err)
	}
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("TryLock: (%v, %v)", ok, err)
	}
	// Deliberately neither unlocked nor closed.
}

// TestAbandonedLockIsReleased is the backstop for a forgotten lock.
//
// An abandoned file watcher wastes a descriptor. An abandoned lock is
// something other processes are actively blocked on, so leaking one is not a
// slow resource drain but a hang somewhere else.
func TestAbandonedLockIsReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abandoned.lock")
	abandonLock(t, path)

	other := newLockAt(t, path)

	deadline := time.Now().Add(30 * time.Second)
	for {
		runtime.GC()

		ok, err := other.TryLock()
		if err != nil {
			t.Fatalf("TryLock: %s", err)
		}
		if ok {
			_ = other.Unlock()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("an abandoned lock was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMechanismIsReported(t *testing.T) {
	if Mechanism() == "" {
		t.Error("Mechanism() is empty")
	}
	t.Logf("locking on this platform uses %s (mandatory: %v)", Mechanism(), IsMandatory())
}

func TestAccessors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accessors.lock")
	// A zero mode must fall back to the default rather than creating a file
	// nobody can open.
	l := newLockAt(t, path, WithFileMode(0), WithDirMode(0))

	if got := l.Path(); got != path {
		t.Errorf("Path() = %q, want %q", got, path)
	}
	if l.Locked() {
		t.Error("Locked() is true before anything was acquired")
	}
	if got := l.Mode(); got != Unlocked {
		t.Errorf("Mode() = %s on a fresh lock, want %s", got, Unlocked)
	}

	if ok, err := l.TryRLock(); err != nil || !ok {
		t.Fatalf("TryRLock: (%v, %v)", ok, err)
	}
	if !l.Locked() {
		t.Error("Locked() is false while a shared lock is held")
	}
	if got := l.Mode(); got != Shared {
		t.Errorf("Mode() = %s while holding shared, want %s", got, Shared)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("Unlock: %s", err)
	}
	if l.Locked() {
		t.Error("Locked() is true after unlocking")
	}
}

func TestModeStrings(t *testing.T) {
	for mode, want := range map[Mode]string{
		Unlocked: "unlocked", Shared: "shared", Exclusive: "exclusive", Mode(9): "Mode(9)",
	} {
		if got := mode.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", uint8(mode), got, want)
		}
	}
}

func TestZeroModesFallBackToDefaults(t *testing.T) {
	if got := fileModeOrDefault(0, defaultFileMode); got != defaultFileMode {
		t.Errorf("fileModeOrDefault(0) = %v, want the default %v", got, defaultFileMode)
	}
	if got := fileModeOrDefault(0o600, defaultFileMode); got != 0o600 {
		t.Errorf("fileModeOrDefault(0o600) = %v, want it preserved", got)
	}
}

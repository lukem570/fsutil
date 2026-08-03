// Package lock provides advisory file locks that work between processes.
//
// A lock is a file that processes agree to coordinate through:
//
//	l, err := lock.New("/var/lib/thing/.lock")
//	if err != nil {
//		return err
//	}
//	defer l.Close()
//
//	if err := l.Lock(ctx); err != nil {
//		return err
//	}
//	defer l.Unlock()
//
// Locks are exclusive by default. [Lock.RLock] takes a shared lock instead,
// which any number of holders may hold at once but which excludes every
// exclusive holder — the usual reader/writer arrangement, extended across
// process boundaries.
//
// # Advisory, not mandatory
//
// On Unix these locks are advisory: they coordinate processes that ask, and do
// nothing whatsoever to a process that simply opens the file and writes. A
// lock is a convention between cooperating programs, not a protection against
// uncooperative ones. Windows locks are mandatory and will fail an unrelated
// process's read or write, so a program that behaves correctly on one may
// behave differently on the other.
//
// # Network filesystems
//
// Locking over NFS, SMB, and FUSE ranges from reliable to entirely absent
// depending on the server, the client, the mount options, and the protocol
// version. None of that is visible from here: taking a lock appears to
// succeed, and two machines may hold the same "exclusive" lock at once. Do not
// use a network filesystem to coordinate between machines. Use something built
// for the job.
//
// # Abandoned locks
//
// A lock whose process exits is released by the operating system, so a crash
// does not leave the file locked forever. A [Lock] that becomes unreachable
// without being unlocked is released by the garbage collector — but see
// [Lock.Close] for why that is a backstop rather than a plan.
package lock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Errors returned by [Lock]. Test for them with [errors.Is].
var (
	// ErrNotHeld is returned by Unlock when the lock is not currently held.
	ErrNotHeld = errors.New("lock: not held")

	// ErrClosed is returned when a method is called on a closed Lock.
	ErrClosed = errors.New("lock: already closed")

	// ErrWouldBlock is returned when a non-blocking acquisition finds the lock
	// held elsewhere. TryLock reports this as false rather than as an error;
	// it appears when a blocking acquisition is given an already-expired
	// context.
	ErrWouldBlock = errors.New("lock: held by another owner")
)

// Mode describes what kind of hold a [Lock] currently has.
type Mode uint8

const (
	// Unlocked means the lock is not held.
	Unlocked Mode = iota
	// Shared means the lock is held for reading, alongside any other shared
	// holders.
	Shared
	// Exclusive means the lock is held for writing, to the exclusion of all
	// others.
	Exclusive
)

func (m Mode) String() string {
	switch m {
	case Unlocked:
		return "unlocked"
	case Shared:
		return "shared"
	case Exclusive:
		return "exclusive"
	default:
		return fmt.Sprintf("Mode(%d)", uint8(m))
	}
}

// Lock is an advisory lock on a file.
//
// A Lock is created with [New] and must be released with [Lock.Close] when no
// longer needed. It is safe for concurrent use: goroutines contending for the
// same Lock are serialised, and contend with other processes only once one of
// them wins locally.
type Lock struct {
	// state holds everything that must be released. It is a separate
	// allocation so that the cleanup registered below can reach the file
	// descriptor without keeping the Lock itself alive, which would stop the
	// cleanup ever running.
	state   *lockState
	cleanup runtime.Cleanup
}

type lockState struct {
	path string
	opts options

	mu     sync.Mutex
	file   *os.File
	mode   Mode
	closed bool

	// local serialises goroutines within this process before any of them
	// competes with other processes. See registry.go.
	local *localLock
}

// New prepares a lock on path, creating the file if it does not exist.
//
// Creating a Lock does not acquire anything; it opens the file and holds it
// open, so that acquiring and releasing later cost no further opens. The file
// is never removed. Deleting a lock file that another process has open would
// let a third process create a new one and take a lock that the second still
// believes it holds, so the file outliving its users is deliberate.
func New(path string, opts ...Option) (*Lock, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt.apply(&o)
	}
	if err := o.validate(); err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("lock: resolving %q: %w", path, err)
	}

	if o.createDirs {
		if err := os.MkdirAll(filepath.Dir(abs), o.dirMode); err != nil {
			return nil, fmt.Errorf("lock: creating directory for %q: %w", abs, err)
		}
	}

	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, o.fileMode)
	if err != nil {
		return nil, fmt.Errorf("lock: opening %q: %w", abs, err)
	}

	// Key the process-local entry on file identity rather than on the path, so
	// that two names for one file — a symlink, a hard link, a relative path
	// reaching the same place — share it. Coordinating by name would let two
	// goroutines believe they were locking different things.
	local, err := localFor(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock: identifying %q: %w", abs, err)
	}

	st := &lockState{path: abs, opts: o, file: file, local: local}
	l := &Lock{state: st}
	l.cleanup = runtime.AddCleanup(l, cleanupAbandoned, st)
	return l, nil
}

// cleanupAbandoned releases a lock that was dropped without being closed.
//
// Teardown runs on its own goroutine because the runtime executes cleanups one
// after another, and releasing a lock can block briefly on the kernel.
//
// This matters more here than for most resources. An abandoned file watcher
// wastes a descriptor; an abandoned lock is something other processes are
// actively waiting on, and until it is released they make no progress.
func cleanupAbandoned(st *lockState) {
	go func() {
		_ = st.close()
	}()
}

// Path returns the absolute path of the lock file.
func (l *Lock) Path() string {
	defer runtime.KeepAlive(l)
	return l.state.path
}

// Mode reports how the lock is currently held.
func (l *Lock) Mode() Mode {
	defer runtime.KeepAlive(l)

	st := l.state
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.mode
}

// Locked reports whether the lock is held in any mode.
func (l *Lock) Locked() bool { return l.Mode() != Unlocked }

// TryLock takes the lock exclusively without waiting.
//
// It reports whether the lock was acquired. A false return means another
// holder — in this process or another — has it, which is an ordinary outcome
// and not an error. An error means the attempt could not be made at all.
func (l *Lock) TryLock() (bool, error) {
	defer runtime.KeepAlive(l)
	return l.state.tryAcquire(Exclusive)
}

// TryRLock takes the lock for shared access without waiting.
//
// It reports whether the lock was acquired. Any number of shared holders may
// hold it at once; a false return means an exclusive holder has it.
func (l *Lock) TryRLock() (bool, error) {
	defer runtime.KeepAlive(l)
	return l.state.tryAcquire(Shared)
}

// Lock takes the lock exclusively, waiting until it is available or ctx is
// done.
//
// Waiting is implemented by retrying, not by blocking in the kernel. That
// costs a little latency — up to one retry interval, which [WithRetryInterval]
// controls — and buys cancellation that actually works: a goroutine parked in
// a blocking lock syscall cannot be interrupted by a context, so a caller who
// cancelled would be left waiting anyway while the goroutine leaked.
func (l *Lock) Lock(ctx context.Context) error {
	defer runtime.KeepAlive(l)
	return l.state.acquire(ctx, Exclusive)
}

// RLock takes the lock for shared access, waiting until it is available or ctx
// is done. See [Lock.Lock] for how waiting works.
func (l *Lock) RLock(ctx context.Context) error {
	defer runtime.KeepAlive(l)
	return l.state.acquire(ctx, Shared)
}

// Unlock releases the lock.
//
// It returns [ErrNotHeld] if the lock is not held, which is usually a sign
// that a deferred Unlock has run twice or that an acquisition failed and its
// error was ignored.
func (l *Lock) Unlock() error {
	defer runtime.KeepAlive(l)
	return l.state.unlock()
}

// Close releases the lock if held and closes the underlying file.
//
// It is idempotent. A Lock that becomes unreachable without being closed is
// released by the garbage collector, so a forgotten lock does not block other
// processes for the life of this one. Do not rely on that: collection happens
// whenever the runtime decides, which may be long after another process began
// waiting, and may never happen in a program that exits promptly. It is a
// backstop for bugs, not a substitute for closing.
func (l *Lock) Close() error {
	defer runtime.KeepAlive(l)

	l.cleanup.Stop()
	return l.state.close()
}

// tryAcquire attempts to take the lock without waiting.
//
// Order matters: the process-local hold is taken first, and the operating
// system lock second. Contending goroutines are therefore resolved in Go,
// where waiting is cheap and cancellable, and only the winner goes on to
// compete with other processes. Doing it the other way round would have every
// goroutine issuing syscalls to discover something this process already knew.
func (st *lockState) tryAcquire(mode Mode) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return false, ErrClosed
	}
	if st.mode != Unlocked {
		// Already held by this Lock. Re-acquiring would deadlock against
		// ourselves on some platforms and silently succeed on others, so it is
		// reported rather than attempted.
		return false, fmt.Errorf("lock: %s is already held %s", st.path, st.mode)
	}

	if !st.local.tryLock(mode) {
		return false, nil
	}

	ok, err := tryLockFile(st.file, mode == Exclusive)
	if err != nil || !ok {
		st.local.unlock(mode)
		if err != nil {
			return false, fmt.Errorf("lock: acquiring %s: %w", st.path, err)
		}
		return false, nil
	}

	st.mode = mode
	if err := st.writeHolderLocked(); err != nil {
		// Failing to record who holds the lock does not invalidate the hold.
		// The metadata is a diagnostic aid, and losing it is not worth
		// surrendering a lock the caller has legitimately acquired.
		st.mode = mode
	}
	return true, nil
}

// acquire waits for the lock, retrying until it is available or ctx is done.
func (st *lockState) acquire(ctx context.Context, mode Mode) error {
	if st.opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, st.opts.timeout)
		defer cancel()
	}

	// The timer is built only once an attempt has failed. Most acquisitions
	// succeed immediately, and creating a timer for them costs an allocation
	// and a runtime registration to describe a wait that never happens.
	delay := st.opts.retryInterval
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		ok, err := st.tryAcquire(mode)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		// Check cancellation only after an attempt, so that a lock which is
		// free right now is taken even if the context is already expiring.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("lock: waiting for %s: %w", st.path, err)
		}

		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("lock: waiting for %s: %w", st.path, ctx.Err())
		}

		// Back off gradually. A tight retry on a lock held for minutes would
		// spend the whole time issuing syscalls that are certain to fail.
		if delay < st.opts.maxRetryInterval {
			delay = min(delay*2, st.opts.maxRetryInterval)
		}
	}
}

func (st *lockState) unlock() error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return ErrClosed
	}
	if st.mode == Unlocked {
		return ErrNotHeld
	}
	return st.releaseLocked()
}

// releaseLocked drops both holds. st.mu must be held.
//
// The operating system lock is released before the process-local one, mirroring
// the order of acquisition in reverse. Releasing locally first would let
// another goroutine in this process take the local hold and then fail against
// a kernel lock this one has not yet dropped, turning a clean handover into a
// spurious retry.
func (st *lockState) releaseLocked() error {
	mode := st.mode
	st.mode = Unlocked

	err := unlockFile(st.file)
	st.local.unlock(mode)

	if err != nil {
		return fmt.Errorf("lock: releasing %s: %w", st.path, err)
	}
	return nil
}

func (st *lockState) close() error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return nil
	}
	st.closed = true

	var err error
	if st.mode != Unlocked {
		err = st.releaseLocked()
	}

	if cerr := st.file.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("lock: closing %s: %w", st.path, cerr)
	}
	st.local.release()
	return err
}

// writeHolderLocked records who holds the lock, when the option is enabled.
// st.mu must be held.
func (st *lockState) writeHolderLocked() error {
	if !st.opts.recordHolder || st.mode != Exclusive {
		return nil
	}
	return writeHolder(st.file)
}

// Holder describes the process that currently holds a lock, when that
// information was recorded. See [WithHolderInfo].
type Holder struct {
	PID      int       `json:"pid"`
	Hostname string    `json:"hostname"`
	Since    time.Time `json:"since"`
}

// Holder reads the holder information recorded in the lock file.
//
// It reports what the file says, which is not necessarily true: the holder may
// have exited since writing it, and a holder that did not enable
// [WithHolderInfo] records nothing at all. Use it to make a waiting message
// informative — "waiting for pid 123 on host foo" — not to decide whether a
// lock is really held. The only sound way to learn that is to try to take it.
func (l *Lock) Holder() (Holder, error) {
	defer runtime.KeepAlive(l)

	st := l.state
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return Holder{}, ErrClosed
	}
	return readHolder(st.file)
}

// Mechanism names the operating system facility used for locking here, such as
// "flock" or "LockFileEx". It is intended for diagnostics and bug reports,
// where knowing which mechanism was in play is often the whole answer.
func Mechanism() string { return lockingKind }

// IsMandatory reports whether the operating system enforces these locks
// against processes that never asked for one.
//
// It is false on Unix and true on Windows, and the difference is behavioural
// rather than a matter of degree: a program that works on Unix because a
// non-participating writer is simply ignored will find that same writer's
// calls failing on Windows. Code that must behave identically on both should
// be written as though every participant cooperates, because on Unix it must.
func IsMandatory() bool { return lockingIsMandatory }

// fileModeOrDefault is used by options.
func fileModeOrDefault(m, def fs.FileMode) fs.FileMode {
	if m == 0 {
		return def
	}
	return m
}

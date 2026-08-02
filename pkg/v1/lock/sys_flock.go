//go:build unix && !aix

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile takes an advisory lock on f without waiting.
//
// flock is used rather than POSIX record locking because ownership belongs to
// the open file description rather than to the process. The difference is not
// academic: with record locks, closing *any* descriptor to a file releases
// every lock the process holds on it, so an unrelated open and close elsewhere
// in the program silently drops a lock another part of it believes it holds.
// That is among the sharpest edges in the POSIX API, and flock does not have
// it.
//
// LOCK_NB makes the call return rather than wait. Waiting is done by the
// caller, which can be cancelled; a thread parked in a blocking flock cannot.
func tryLockFile(f *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}

	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EWOULDBLOCK):
			// EWOULDBLOCK and EAGAIN are the same value on every Unix, and
			// both mean the lock is held elsewhere. That is an answer, not a
			// failure.
			return false, nil
		case errors.Is(err, unix.EINTR):
			// The Go runtime preempts goroutines with signals, so an
			// interrupted syscall here is routine.
			continue
		default:
			return false, err
		}
	}
}

func unlockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

// lockingKind names the mechanism in use, for diagnostics.
const lockingKind = "flock"

// lockingIsMandatory reports whether the operating system enforces these locks
// against processes that do not participate. On Unix it never does.
const lockingIsMandatory = false

//go:build aix

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// AIX has no flock(2), so POSIX record locking is the only option here.
//
// Record locks carry a caveat sharp enough to be worth stating plainly:
// ownership belongs to the *process*, not to the open file description. Two
// descriptors in one process do not contend — the second acquisition simply
// succeeds — and closing any one descriptor to the file releases every lock
// the process holds on it, so an unrelated open and close elsewhere in the
// program silently drops a lock another part of it believes it still has.
//
// Neither hazard reaches a caller of this package. Goroutines are serialised
// by the process-local registry before any of them reaches the kernel, so at
// most one descriptor per file is ever locked, and that descriptor is opened
// once by New and closed only by Close. What remains is a lock that behaves
// correctly between processes, which is what it is for.

func recordLock(f *os.File, typ int16) error {
	lk := unix.Flock_t{
		Type:   typ,
		Whence: 0, // SEEK_SET
		Start:  0,
		Len:    0, // to end of file, however long it becomes
	}
	return unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lk)
}

func tryLockFile(f *os.File, exclusive bool) (bool, error) {
	typ := int16(unix.F_RDLCK)
	if exclusive {
		typ = int16(unix.F_WRLCK)
	}

	for {
		err := recordLock(f, typ)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EACCES):
			// Both mean the lock is held elsewhere; which one is returned is
			// unspecified. That is an answer, not a failure.
			return false, nil
		case errors.Is(err, unix.EINTR):
			continue
		default:
			return false, err
		}
	}
}

func unlockFile(f *os.File) error {
	for {
		err := recordLock(f, int16(unix.F_UNLCK))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

// lockingKind names the mechanism in use, for diagnostics.
const lockingKind = "fcntl"

// lockingIsMandatory reports whether the operating system enforces these locks
// against processes that do not participate.
const lockingIsMandatory = false

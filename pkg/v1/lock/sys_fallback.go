//go:build !unix && !windows

package lock

import (
	"errors"
	"os"
	"path/filepath"
)

// This implementation covers the platforms with no advisory locking syscall at
// all: Plan 9, and the WebAssembly targets.
//
// It falls back to the oldest trick there is — creating a file that must not
// already exist. Exclusive creation is atomic on every filesystem worth the
// name, which is enough to make it a lock.
//
// It is weaker than a kernel lock in one way that matters, and the difference
// cannot be papered over: the operating system does not release it when the
// holder dies. A process killed while holding one leaves the guard file
// behind, and every later attempt fails against a holder that no longer
// exists. There is no sound automatic remedy — a guard file left by a crash
// and one held by a busy process are indistinguishable — so recovery is a
// human decision and the guard file is left for a human to remove.

// fileKey identifies a lock by path.
//
// The platforms reached here expose no stable file identity, so two names for
// one file are treated as two locks. Within a process that is a real
// limitation; between processes the guard file still serialises them, because
// the guard's own name is derived from the path either way.
type fileKey struct{ path string }

func keyOf(f *os.File) (fileKey, error) {
	abs, err := filepath.Abs(f.Name())
	if err != nil {
		return fileKey{}, err
	}
	return fileKey{path: abs}, nil
}

// guardPath is the file whose existence signifies the lock.
//
// It is a sibling of the lock file rather than the lock file itself, because
// the lock file is created by New and would therefore always exist.
func guardPath(f *os.File) string { return f.Name() + ".held" }

// tryLockFile takes the lock by creating the guard file.
//
// Shared locks are granted as exclusive ones. Exclusive creation cannot
// express "several holders, but no writer", and granting a stricter lock than
// asked for is always safe: it costs concurrency, never correctness.
func tryLockFile(f *os.File, _ bool) (bool, error) {
	guard, err := os.OpenFile(guardPath(f), os.O_CREATE|os.O_EXCL|os.O_RDWR, defaultFileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, guard.Close()
}

func unlockFile(f *os.File) error {
	err := os.Remove(guardPath(f))
	if errors.Is(err, os.ErrNotExist) {
		// Someone removed it already. The lock is not held, which is what
		// unlocking was for.
		return nil
	}
	return err
}

// lockingKind names the mechanism in use, for diagnostics.
const lockingKind = "exclusive-create"

// lockingIsMandatory reports whether the operating system enforces these locks
// against processes that do not participate. Nothing enforces this one.
const lockingIsMandatory = false

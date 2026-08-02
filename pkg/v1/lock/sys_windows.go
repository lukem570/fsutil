//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// fileKey identifies a file independently of the name used to reach it.
//
// The volume serial number and file index together are Windows' equivalent of
// a device and inode pair. They are read from an open handle because the same
// file may be reachable by several paths, and comparing paths would treat
// those as different files.
type fileKey struct {
	volume uint32
	index  uint64
}

func keyOf(f *os.File) (fileKey, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return fileKey{}, err
	}
	return fileKey{
		volume: info.VolumeSerialNumber,
		index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

// lockRegionLength is how much of the file is locked.
//
// Windows locks byte ranges rather than files, so a range has to be chosen.
// The whole 64-bit space is used, which makes the lock behave like a
// whole-file lock and, more importantly, means a lock taken by one version of
// a program overlaps one taken by another. A range chosen to match the file's
// current length would not: two processes locking a file of different apparent
// sizes could each succeed.
const lockRegionLength = ^uint32(0)

func tryLockFile(f *os.File, exclusive bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}

	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0,
		lockRegionLength, lockRegionLength, &overlapped)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		// The lock is held elsewhere. That is an answer, not a failure.
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0,
		lockRegionLength, lockRegionLength, &overlapped)
}

// lockingKind names the mechanism in use, for diagnostics.
const lockingKind = "LockFileEx"

// lockingIsMandatory reports whether the operating system enforces these locks
// against processes that do not participate.
//
// Windows does. A process that never asks for the lock will still have its
// reads and writes fail inside a locked range, which is a real behavioural
// difference from Unix rather than a stricter shade of the same thing: code
// that works on Unix because a non-participating writer is simply ignored will
// see that writer fail here.
const lockingIsMandatory = true

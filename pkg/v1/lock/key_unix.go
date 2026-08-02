//go:build unix

package lock

import (
	"errors"
	"os"
	"syscall"
)

// fileKey identifies a file independently of the name used to reach it.
//
// The device is part of the identity because inode numbers are unique only
// within a filesystem, so two files on different mounts routinely share one.
type fileKey struct {
	dev uint64
	ino uint64
}

func keyOf(f *os.File) (fileKey, error) {
	info, err := f.Stat()
	if err != nil {
		return fileKey{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileKey{}, errors.New("no underlying stat information")
	}
	return fileKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

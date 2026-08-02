//go:build unix

package notify

import (
	"io/fs"
	"syscall"
)

// fileIDSupported reports whether this platform can identify a file
// independently of its name.
const fileIDSupported = true

// fileID identifies a file uniquely, so that the same file can be recognised
// after it has been renamed.
//
// The device number is part of the identity because inode numbers are only
// unique within a filesystem: two files on different mounts routinely share
// one. Comparing inodes alone would pair unrelated files across a mount point.
type fileID struct {
	dev uint64
	ino uint64
}

// statID extracts a file's identity from the result of a stat call. It reports
// false when the information is unavailable, which happens for synthetic
// [fs.FileInfo] values that carry no underlying stat structure.
func statID(info fs.FileInfo) (fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileID{}, false
	}
	// Widths vary by platform: Dev is signed on the BSDs and unsigned on
	// Linux, and both are narrower than 64 bits on some architectures. The
	// conversions are for uniformity of storage, not reinterpretation.
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

//go:build dragonfly || freebsd || netbsd || openbsd

package notify

import "golang.org/x/sys/unix"

// kqOpenFlags opens a path for the sole purpose of watching it.
//
// The BSDs have no equivalent of Darwin's O_EVTONLY, so a read descriptor is
// the least that will do. It is never read from.
//
// O_NONBLOCK is needed because opening a named pipe for reading otherwise
// blocks until a writer appears, which would hang a caller who happened to
// watch a directory containing a fifo.
const kqOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK

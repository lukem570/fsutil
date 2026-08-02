//go:build darwin

package notify

import "golang.org/x/sys/unix"

// kqOpenFlags opens a path for the sole purpose of watching it.
//
// O_EVTONLY is specific to Darwin and is exactly what is wanted here: it
// requests a descriptor usable for event notification without taking a usage
// reference on the underlying filesystem. An ordinary read descriptor would
// keep a volume busy, so watching a directory on an external disk would
// silently prevent it from being ejected.
//
// O_NONBLOCK is needed because opening a named pipe for reading otherwise
// blocks until a writer appears, which would hang a caller who happened to
// watch a directory containing a fifo.
const kqOpenFlags = unix.O_EVTONLY | unix.O_CLOEXEC | unix.O_NONBLOCK

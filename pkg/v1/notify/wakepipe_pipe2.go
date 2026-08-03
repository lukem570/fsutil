//go:build linux || dragonfly || freebsd || netbsd || openbsd

package notify

import "golang.org/x/sys/unix"

// newWakePipe creates the pipe used to interrupt a backend blocked in the
// kernel.
//
// Shutdown cannot simply close the descriptor a reader is waiting on: a thread
// parked in poll or kevent does not reliably observe its descriptor closing,
// and the number can be reused by another goroutine in the meantime, so the
// wait might never end or might end for the wrong reason. A second descriptor
// that the reader also waits on gives shutdown something unambiguous to say.
//
// Both ends are close-on-exec so that a forked child cannot hold the pipe open
// and keep a reader alive, and non-blocking so that signalling shutdown cannot
// itself block if the pipe is somehow full.
// The build constraint lists exactly the platforms with a backend that needs
// it; adding one elsewhere before its backend exists leaves it unreferenced.
func newWakePipe(p *[2]int) error {
	return unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK)
}

//go:build darwin

package notify

import "golang.org/x/sys/unix"

// newWakePipe creates the pipe used to interrupt a backend blocked in the
// kernel. See the documentation on the other implementation of this function
// for why a pipe is needed at all.
//
// Darwin has no pipe2, so the flags that other platforms set atomically at
// creation have to be applied afterwards. The window between the two is
// harmless here: the descriptors are not published to any other goroutine
// until this function returns, so there is nothing that could exec or read in
// between.
func newWakePipe(p *[2]int) error {
	if err := unix.Pipe(p[:]); err != nil {
		return err
	}
	for _, fd := range p {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
			_ = unix.Close(p[0])
			_ = unix.Close(p[1])
			return err
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, unix.O_NONBLOCK); err != nil {
			_ = unix.Close(p[0])
			_ = unix.Close(p[1])
			return err
		}
	}
	return nil
}

//go:build linux

package notify

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
)

// The inotify backend reads change notifications from a single kernel file
// descriptor.
//
// Two properties of inotify shape everything below.
//
// First, inotify watches *inodes*, not names. A watch added for a path follows
// the file that path currently refers to. If the file is replaced — which is
// what an editor does when it saves by writing a temporary and renaming it
// over the original — the watch stays with the file that has been moved aside
// and reports nothing about the new occupant of the name. This is why watching
// a directory is almost always right and watching a file almost always wrong,
// and it is a property of the kernel interface rather than a decision made
// here.
//
// Second, a watch descriptor is a shared, finite kernel resource, bounded by
// fs.inotify.max_user_watches per user. Exhausting it is the single most
// common failure in production, so the error for it names the sysctl.

// inotifyReadBufferSize bounds a single read from the inotify descriptor.
//
// The kernel refuses to return a partial event, so the buffer must exceed the
// largest possible one: the 16-byte header plus a name of up to NAME_MAX. 64
// KiB holds several hundred typical events, which keeps the number of read
// syscalls low under load without making the allocation notable.
const inotifyReadBufferSize = 64 * 1024

// inotifyHeaderSize is the size of the fixed part of a kernel event: the watch
// descriptor, mask, and cookie as 32-bit values, followed by the name length.
const inotifyHeaderSize = 16

func init() {
	factoryCaps[BackendINotify] = CapUnportableOps | CapNoFollow | CapPreciseRename | CapFDPerDir

	register(backendFactory{
		kind:      BackendINotify,
		priority:  100,
		available: inotifyAvailable,
		new:       newInotifyBackend,
	})
}

// inotifyAvailable reports whether this kernel will give us an inotify
// instance.
//
// It probes rather than assuming, because compiling for Linux does not
// guarantee inotify works: a sandbox may forbid the syscall, and
// fs.inotify.max_user_instances may already be exhausted by other processes.
func inotifyAvailable() bool {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}

// inWatch is one watch descriptor and the names registered against it.
//
// There can be more than one name. inotify keys on the inode, so adding two
// paths that resolve to the same file — hard links, or the same directory
// reached through a symlink — yields the same descriptor. Tracking the names
// lets Remove drop just one of them without tearing down a watch the caller
// still wants.
type inWatch struct {
	wd    int32
	names []string // names[0] is the name events are reported under
	opts  addOpts
}

type inotifyBackend struct {
	sink sink

	fd int // the inotify instance

	// wake is a pipe whose only purpose is to interrupt the blocking poll in
	// the read loop. The inotify descriptor has nothing to say at shutdown, so
	// without a second descriptor to watch, Close could not get the reader's
	// attention.
	wake [2]int

	mu     sync.Mutex
	byWD   map[int32]*inWatch
	byName map[string]int32
	closed bool

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func newInotifyBackend(s sink, _ config) (backend, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("creating inotify instance: %w", inotifyInitError(err))
	}

	b := &inotifyBackend{
		sink:   s,
		fd:     fd,
		byWD:   make(map[int32]*inWatch),
		byName: make(map[string]int32),
	}

	if err := newWakePipe(&b.wake); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("creating shutdown pipe: %w", err)
	}

	b.wg.Add(1)
	go b.read()
	return b, nil
}

func (b *inotifyBackend) Kind() Backend { return BackendINotify }

func (b *inotifyBackend) Capabilities() Capability { return factoryCaps[BackendINotify] }

func (b *inotifyBackend) Add(path string, opts addOpts) error {
	mask := inotifyMask(opts)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	// Adding an existing watch replaces its mask, which is what re-adding a
	// path with different options should do.
	wd, err := unix.InotifyAddWatch(b.fd, path, mask)
	if err != nil {
		return inotifyAddError(path, err)
	}

	w, ok := b.byWD[int32(wd)]
	if !ok {
		w = &inWatch{wd: int32(wd)}
		b.byWD[int32(wd)] = w
	}
	w.opts = opts
	if !slices.Contains(w.names, path) {
		w.names = append(w.names, path)
	}
	b.byName[path] = int32(wd)
	return nil
}

func (b *inotifyBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	wd, ok := b.byName[path]
	if !ok {
		return ErrNonExistentWatch
	}
	delete(b.byName, path)

	w := b.byWD[wd]
	if w == nil {
		return ErrNonExistentWatch
	}
	w.names = slices.DeleteFunc(w.names, func(n string) bool { return n == path })
	if len(w.names) > 0 {
		// Another name still refers to this inode, so the kernel watch must
		// stay. Tearing it down here would silently stop events the caller is
		// still expecting for the other name.
		return nil
	}

	delete(b.byWD, wd)
	if _, err := unix.InotifyRmWatch(b.fd, uint32(wd)); err != nil {
		// EINVAL means the kernel has already dropped this watch, which
		// happens when the watched file was deleted a moment ago. The caller
		// asked for it to be gone; it is gone.
		if !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("removing watch: %w", err)
		}
	}
	return nil
}

func (b *inotifyBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.byName))
	for name := range b.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (b *inotifyBackend) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()

		// Wake the reader before closing anything it is using. Closing a
		// descriptor that a thread is blocked in poll() on is not reliably
		// observed by that thread, and the number may be reused underneath it.
		if _, err := unix.Write(b.wake[1], []byte{0}); err != nil && !errors.Is(err, unix.EAGAIN) {
			b.closeErr = fmt.Errorf("signalling shutdown: %w", err)
		}

		b.wg.Wait()

		// Only now is nothing using these. Closing the inotify descriptor
		// releases every watch it holds.
		_ = unix.Close(b.fd)
		_ = unix.Close(b.wake[0])
		_ = unix.Close(b.wake[1])
	})
	return b.closeErr
}

// read is the event loop. It blocks in poll until either the kernel has events
// or Close asks it to stop.
func (b *inotifyBackend) read() {
	defer b.wg.Done()

	buf := make([]byte, inotifyReadBufferSize)
	fds := []unix.PollFd{
		{Fd: int32(b.fd), Events: unix.POLLIN},
		{Fd: int32(b.wake[0]), Events: unix.POLLIN},
	}

	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			// The Go runtime preempts goroutines with signals, so an
			// interrupted syscall here is routine rather than exceptional.
			if errors.Is(err, unix.EINTR) {
				continue
			}
			b.sink.fail(fmt.Errorf("notify: waiting for events: %w", err))
			return
		}

		if fds[1].Revents != 0 {
			return // Close asked us to stop
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}

		n, err := unix.Read(b.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			if !b.sink.fail(fmt.Errorf("notify: reading events: %w", err)) {
				return
			}
			continue
		}
		if n <= 0 {
			continue
		}

		if !b.dispatch(buf[:n]) {
			return
		}
	}
}

// dispatch decodes a batch of kernel events and delivers them. It reports
// false when the watcher is shutting down.
func (b *inotifyBackend) dispatch(buf []byte) bool {
	for len(buf) >= inotifyHeaderSize {
		wd := int32(binary.NativeEndian.Uint32(buf[0:4]))
		mask := binary.NativeEndian.Uint32(buf[4:8])
		nameLen := binary.NativeEndian.Uint32(buf[12:16])

		if uint64(len(buf)) < uint64(inotifyHeaderSize)+uint64(nameLen) {
			// The kernel does not split events across reads, so this cannot
			// happen; treat it as corruption rather than looping forever.
			b.sink.fail(fmt.Errorf("notify: truncated event from kernel (%d bytes left, name length %d)",
				len(buf), nameLen))
			return true
		}

		// The name is NUL-padded to an alignment boundary, so the padding is
		// part of the record but not part of the name.
		name := buf[inotifyHeaderSize : inotifyHeaderSize+nameLen]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		buf = buf[inotifyHeaderSize+nameLen:]

		if !b.handle(wd, mask, string(name)) {
			return false
		}
	}
	return true
}

// handle turns one kernel event into zero or more delivered events.
func (b *inotifyBackend) handle(wd int32, mask uint32, name string) bool {
	if debugEnabled {
		debugf("inotify wd=%d mask=%s name=%q", wd, inotifyMaskString(mask), name)
	}

	// An overflow is reported against no watch in particular. Events have been
	// lost: watching continues, but the caller's picture of the tree is now
	// incomplete and only a rescan can repair it.
	if mask&unix.IN_Q_OVERFLOW != 0 {
		return b.sink.fail(fmt.Errorf("notify: %w: the kernel event queue overflowed and changes were lost; "+
			"drain the Events channel faster or raise fs.inotify.max_queued_events", ErrEventOverflow))
	}

	b.mu.Lock()
	w := b.byWD[wd]
	var (
		watchName string
		opts      addOpts
	)
	if w != nil && len(w.names) > 0 {
		watchName, opts = w.names[0], w.opts
	}

	// IN_IGNORED is the kernel's confirmation that a watch is gone, whether
	// because the file was deleted or because we removed it. Either way the
	// descriptor is now invalid and may be reissued, so our bookkeeping must
	// drop it before it is mistaken for a live watch.
	if mask&unix.IN_IGNORED != 0 && w != nil {
		delete(b.byWD, wd)
		for _, n := range w.names {
			delete(b.byName, n)
		}
	}
	b.mu.Unlock()

	if w == nil {
		// An event for a watch we have already forgotten. This is expected:
		// events queued before a Remove are still delivered afterwards.
		return true
	}
	if mask&unix.IN_IGNORED != 0 {
		return true // bookkeeping only; not a change to report
	}

	op := inotifyOps(mask) & opts.ops
	if op == 0 {
		return true
	}

	// An empty name means the event is about the watched path itself rather
	// than something inside it.
	path := watchName
	if name != "" {
		path = filepath.Join(watchName, name)
	}

	return b.sink.send(Event{Name: path, Op: op})
}

// inotifyMask translates the requested operations into a kernel mask.
func inotifyMask(opts addOpts) uint32 {
	var m uint32

	if opts.ops.Has(Create) {
		// A file moved into a watched directory is, from the directory's point
		// of view, a new entry appearing.
		m |= unix.IN_CREATE | unix.IN_MOVED_TO
	}
	if opts.ops.Has(Write) {
		m |= unix.IN_MODIFY
	}
	if opts.ops.Has(Remove) {
		m |= unix.IN_DELETE | unix.IN_DELETE_SELF
	}
	if opts.ops.Has(Rename) {
		m |= unix.IN_MOVED_FROM | unix.IN_MOVE_SELF
	}
	if opts.ops.Has(Chmod) {
		m |= unix.IN_ATTRIB
	}
	if opts.ops.Has(UnportableOpen) {
		m |= unix.IN_OPEN
	}
	if opts.ops.Has(UnportableRead) {
		m |= unix.IN_ACCESS
	}
	if opts.ops.Has(UnportableCloseWrite) {
		m |= unix.IN_CLOSE_WRITE
	}
	if opts.ops.Has(UnportableCloseRead) {
		m |= unix.IN_CLOSE_NOWRITE
	}

	// Without this, a file that has been unlinked but is still held open keeps
	// producing events under a name that no longer exists. No other backend
	// can see such a file at all, so excluding it keeps behaviour consistent
	// across platforms as well as being less confusing on its own terms.
	m |= unix.IN_EXCL_UNLINK

	if opts.noFollow {
		m |= unix.IN_DONT_FOLLOW
	}
	return m
}

// inotifyOps translates a kernel mask into the operations it represents.
func inotifyOps(mask uint32) Op {
	var op Op

	if mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
		op |= Create
	}
	if mask&unix.IN_MODIFY != 0 {
		op |= Write
	}
	if mask&(unix.IN_DELETE|unix.IN_DELETE_SELF) != 0 {
		op |= Remove
	}
	// A rename reports the name that was vacated; the name that was filled is
	// reported separately as a creation, which is how every other backend in
	// this package describes the same event.
	if mask&(unix.IN_MOVED_FROM|unix.IN_MOVE_SELF) != 0 {
		op |= Rename
	}
	if mask&unix.IN_ATTRIB != 0 {
		op |= Chmod
	}
	if mask&unix.IN_OPEN != 0 {
		op |= UnportableOpen
	}
	if mask&unix.IN_ACCESS != 0 {
		op |= UnportableRead
	}
	if mask&unix.IN_CLOSE_WRITE != 0 {
		op |= UnportableCloseWrite
	}
	if mask&unix.IN_CLOSE_NOWRITE != 0 {
		op |= UnportableCloseRead
	}
	return op
}

// inotifyInitError explains the failures that have an operator-level cause.
func inotifyInitError(err error) error {
	if errors.Is(err, unix.EMFILE) {
		return fmt.Errorf("%w: the per-user limit on inotify instances is exhausted; "+
			"raise fs.inotify.max_user_instances", err)
	}
	if errors.Is(err, unix.ENFILE) {
		return fmt.Errorf("%w: the system-wide open file limit is exhausted", err)
	}
	return err
}

// inotifyAddError explains the failures that have an operator-level cause.
//
// A bare ENOSPC here is one of the least helpful errors in Linux: it does not
// mean the disk is full, it means the watch limit is reached, and the fix is a
// sysctl the caller has probably never heard of.
func inotifyAddError(path string, err error) error {
	switch {
	case errors.Is(err, unix.ENOSPC):
		return fmt.Errorf("%w: the per-user limit on inotify watches is exhausted; "+
			"raise fs.inotify.max_user_watches or watch fewer directories", err)
	case errors.Is(err, unix.ENOENT):
		return &os.PathError{Op: "inotify_add_watch", Path: path, Err: unix.ENOENT}
	case errors.Is(err, unix.EACCES):
		return &os.PathError{Op: "inotify_add_watch", Path: path, Err: unix.EACCES}
	default:
		return err
	}
}

// inotifyMaskString renders a kernel mask for tracing.
func inotifyMaskString(mask uint32) string {
	bits := []struct {
		bit  uint32
		name string
	}{
		{unix.IN_ACCESS, "ACCESS"},
		{unix.IN_MODIFY, "MODIFY"},
		{unix.IN_ATTRIB, "ATTRIB"},
		{unix.IN_CLOSE_WRITE, "CLOSE_WRITE"},
		{unix.IN_CLOSE_NOWRITE, "CLOSE_NOWRITE"},
		{unix.IN_OPEN, "OPEN"},
		{unix.IN_MOVED_FROM, "MOVED_FROM"},
		{unix.IN_MOVED_TO, "MOVED_TO"},
		{unix.IN_CREATE, "CREATE"},
		{unix.IN_DELETE, "DELETE"},
		{unix.IN_DELETE_SELF, "DELETE_SELF"},
		{unix.IN_MOVE_SELF, "MOVE_SELF"},
		{unix.IN_UNMOUNT, "UNMOUNT"},
		{unix.IN_Q_OVERFLOW, "Q_OVERFLOW"},
		{unix.IN_IGNORED, "IGNORED"},
		{unix.IN_ISDIR, "ISDIR"},
	}

	var b bytes.Buffer
	for _, x := range bits {
		if mask&x.bit != 0 {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(x.name)
		}
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "0x%x", mask)
	}
	return b.String()
}

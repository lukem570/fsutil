//go:build linux

package notify

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
)

// The fanotify backend reads change notifications through Linux's fanotify
// interface.
//
// It is not a better inotify, and it is not chosen automatically. It requires
// CAP_SYS_ADMIN, and a backend that works only because the process happens to
// be running as root is not something to depend on silently — so it is
// selected only when asked for by name. What it offers in exchange is the
// ability to watch a whole mount or filesystem with a single mark, where
// inotify would need one watch per directory and would run out.
//
// Reporting which *name* changed requires the kernel to identify files by
// handle rather than by descriptor, which is what FAN_REPORT_DFID_NAME does
// and why this backend needs Linux 5.9 or newer. Without it, fanotify can say
// that something was modified but not that a file was created — which is not
// enough to be a general watcher.
//
// Resolving those handles could be done with open_by_handle_at, but that needs
// CAP_DAC_READ_SEARCH on top of everything else. It is avoided entirely here:
// the handle of each marked directory is recorded when the mark is placed, and
// an event's directory handle is matched against those. Since events arrive
// only for directories this backend marked, the match always succeeds, and the
// path is the marked directory joined with the name the kernel supplied.

// Layout of struct fanotify_event_metadata, read field by field so that no
// unsafe conversion is needed.
const (
	fanMetaEventLen   = 0  // __u32
	fanMetaVersion    = 4  // __u8
	fanMetaMetaLen    = 6  // __u16
	fanMetaMask       = 8  // __u64
	fanMetaFD         = 16 // __s32
	fanMetaHeaderSize = 24
)

// Layout of struct fanotify_event_info_header, and of the fid record following
// it: a filesystem identifier, then a struct file_handle, then — for the
// DFID_NAME record type — a NUL-terminated name.
const (
	fanInfoType       = 0 // __u8
	fanInfoLen        = 2 // __u16
	fanInfoHeaderSize = 4

	fanFSIDSize = 8 // __kernel_fsid_t

	fanHandleBytes = 0 // __u32
	fanHandleType  = 4 // __s32
	fanHandleData  = 8
)

// fanReadBufferSize bounds a single read. Events are larger than inotify's
// because each carries a file handle and a name.
const fanReadBufferSize = 64 * 1024

func init() {
	factoryCaps[BackendFANotify] = CapUnportableOps | CapPreciseRename | CapPrivileged

	register(backendFactory{
		kind: BackendFANotify,
		// Ranked above inotify for when it is asked for by name. Automatic
		// selection skips it regardless, because it is marked privileged.
		priority:  110,
		available: fanotifyAvailable,
		new:       newFanotifyBackend,
	})
}

// fanotifyInitFlags requests notification-only events identified by directory
// handle and name.
//
// FAN_CLASS_NOTIF is deliberate: the alternative classes let a program hold up
// every file access on the system until it answers, which is a different and
// far more dangerous kind of tool than a file watcher.
const fanotifyInitFlags = unix.FAN_CLASS_NOTIF | unix.FAN_REPORT_DFID_NAME |
	unix.FAN_CLOEXEC | unix.FAN_NONBLOCK

// fanotifyAvailable reports whether this kernel and this process can use
// fanotify in the way this backend needs.
//
// The probe is the initialisation call itself. Nothing else is conclusive: the
// kernel version does not tell you whether the process has CAP_SYS_ADMIN, and
// the capability does not tell you whether the kernel is new enough for
// handle-and-name reporting.
func fanotifyAvailable() bool {
	fd, err := unix.FanotifyInit(fanotifyInitFlags, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}

// fanWatch is one marked directory.
type fanWatch struct {
	path string
	opts addOpts

	// dir is the directory actually marked, and filter the single name this
	// watch cares about within it.
	//
	// fanotify marks directories. A watch on a file is therefore a mark on its
	// parent with everything else discarded — the same arrangement the Windows
	// backend uses, and for the same reason: the alternative is refusing a
	// perfectly reasonable request because of how the interface is shaped.
	dir    string
	filter string

	// handleType and handle are what the kernel will use to refer to this
	// directory in events. They are recorded here so that an event can be
	// matched back to a path without asking the kernel to resolve anything.
	handleType int32
	handle     []byte
}

type fanotifyBackend struct {
	sink sink

	fd   int
	wake [2]int

	mu      sync.Mutex
	watches map[string]*fanWatch
	closed  bool

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func newFanotifyBackend(s sink, _ config) (backend, error) {
	fd, err := unix.FanotifyInit(fanotifyInitFlags, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return nil, fanotifyInitError(err)
	}

	b := &fanotifyBackend{
		sink:    s,
		fd:      fd,
		watches: make(map[string]*fanWatch),
	}

	if err := newWakePipe(&b.wake); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("creating shutdown pipe: %w", err)
	}

	b.wg.Add(1)
	go b.read()
	return b, nil
}

func (b *fanotifyBackend) Kind() Backend { return BackendFANotify }

func (b *fanotifyBackend) Capabilities() Capability { return factoryCaps[BackendFANotify] }

func (b *fanotifyBackend) Add(path string, opts addOpts) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	dir, filter := path, ""
	if !info.IsDir() {
		dir, filter = filepath.Dir(path), filepath.Base(path)
	}

	// Record how the kernel will name this directory in events. Doing it now,
	// once, is what makes resolving events later a comparison in memory rather
	// than a privileged syscall per event.
	handle, _, err := unix.NameToHandleAt(unix.AT_FDCWD, dir, 0)
	if err != nil {
		return fmt.Errorf("identifying %s: %w", dir, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	mask := fanotifyMask(opts.ops)
	if err := unix.FanotifyMark(b.fd, unix.FAN_MARK_ADD, mask, unix.AT_FDCWD, dir); err != nil {
		return fanotifyMarkError(dir, err)
	}

	b.watches[path] = &fanWatch{
		path:       path,
		opts:       opts,
		dir:        dir,
		filter:     filter,
		handleType: handle.Type(),
		handle:     append([]byte(nil), handle.Bytes()...),
	}
	return nil
}

func (b *fanotifyBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	w := b.watches[path]
	if w == nil {
		return ErrNonExistentWatch
	}
	delete(b.watches, path)

	// Another watch may still need this directory marked — a file watch and a
	// watch on its parent share one mark — so it is only unmarked once nothing
	// refers to it.
	for _, other := range b.watches {
		if other.dir == w.dir {
			return nil
		}
	}

	mask := fanotifyMask(w.opts.ops)
	if err := unix.FanotifyMark(b.fd, unix.FAN_MARK_REMOVE, mask, unix.AT_FDCWD, w.dir); err != nil {
		// ENOENT means the kernel has already dropped the mark, which is what
		// happens when the directory itself is gone. The caller asked for it
		// to stop being watched; it has.
		if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("removing mark on %s: %w", path, err)
		}
	}
	return nil
}

func (b *fanotifyBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.watches))
	for path := range b.watches {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (b *fanotifyBackend) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()

		if _, err := unix.Write(b.wake[1], []byte{0}); err != nil && !errors.Is(err, unix.EAGAIN) {
			b.closeErr = fmt.Errorf("signalling shutdown: %w", err)
		}

		b.wg.Wait()

		_ = unix.Close(b.fd)
		_ = unix.Close(b.wake[0])
		_ = unix.Close(b.wake[1])
	})
	return b.closeErr
}

func (b *fanotifyBackend) read() {
	defer b.wg.Done()

	buf := make([]byte, fanReadBufferSize)
	fds := []unix.PollFd{
		{Fd: int32(b.fd), Events: unix.POLLIN},
		{Fd: int32(b.wake[0]), Events: unix.POLLIN},
	}

	for {
		if _, err := unix.Poll(fds, -1); err != nil {
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

// dispatch decodes a batch of kernel events and delivers them.
func (b *fanotifyBackend) dispatch(buf []byte) bool {
	for len(buf) >= fanMetaHeaderSize {
		eventLen := binary.NativeEndian.Uint32(buf[fanMetaEventLen:])
		if uint64(eventLen) < fanMetaHeaderSize || uint64(eventLen) > uint64(len(buf)) {
			b.sink.fail(fmt.Errorf("notify: malformed fanotify event of length %d", eventLen))
			return true
		}

		event := buf[:eventLen]
		buf = buf[eventLen:]

		if version := event[fanMetaVersion]; version != unix.FANOTIFY_METADATA_VERSION {
			b.sink.fail(fmt.Errorf("notify: fanotify version %d, expected %d; "+
				"the running kernel is not the one this was built against",
				version, unix.FANOTIFY_METADATA_VERSION))
			return true
		}

		mask := binary.NativeEndian.Uint64(event[fanMetaMask:])

		// FAN_REPORT_FID leaves no descriptor to close, but a kernel that
		// supplied one anyway must not have it leaked.
		if fd := int32(binary.NativeEndian.Uint32(event[fanMetaFD:])); fd >= 0 {
			_ = unix.Close(int(fd))
		}

		if mask&unix.FAN_Q_OVERFLOW != 0 {
			if !b.sink.fail(fmt.Errorf("notify: %w: the fanotify queue overflowed and changes were lost",
				ErrEventOverflow)) {
				return false
			}
			continue
		}

		metaLen := binary.NativeEndian.Uint16(event[fanMetaMetaLen:])
		if uint64(metaLen) > uint64(len(event)) {
			continue
		}
		if !b.handle(mask, event[metaLen:]) {
			return false
		}
	}
	return true
}

// handle turns one kernel event into a delivered event, resolving its path
// from the information records that follow the metadata.
func (b *fanotifyBackend) handle(mask uint64, info []byte) bool {
	matched, name, ok := b.resolve(info)
	if !ok {
		// An event for a directory this backend did not mark, or one whose
		// records could not be read. Neither can be attributed to a path, and
		// guessing would be worse than silence.
		return true
	}

	if debugEnabled {
		debugf("fanotify mask=0x%x name=%q watches=%d", mask, name, len(matched))
	}

	for _, w := range matched {
		// A watch on a single file sees everything happening in its parent, so
		// everything else is discarded here.
		if w.filter != "" && w.filter != name {
			continue
		}

		op := fanotifyOps(mask) & w.opts.ops
		if op == 0 {
			continue
		}

		path := w.dir
		if name != "" && name != "." {
			path = filepath.Join(w.dir, name)
		}
		if !b.sink.send(Event{Name: path, Op: op}) {
			return false
		}
	}
	return true
}

// resolve finds which marked directory an event refers to, and the name within
// it, by matching the directory handle the kernel supplied against the handles
// recorded when the marks were placed.
func (b *fanotifyBackend) resolve(info []byte) (matched []*fanWatch, name string, ok bool) {
	for len(info) >= fanInfoHeaderSize {
		recordLen := int(binary.NativeEndian.Uint16(info[fanInfoLen:]))
		if recordLen < fanInfoHeaderSize || recordLen > len(info) {
			return nil, "", false
		}

		record := info[:recordLen]
		info = info[recordLen:]

		switch record[fanInfoType] {
		case unix.FAN_EVENT_INFO_TYPE_DFID_NAME, unix.FAN_EVENT_INFO_TYPE_DFID:
		default:
			continue
		}

		body := record[fanInfoHeaderSize:]
		if len(body) < fanFSIDSize+fanHandleData {
			continue
		}
		body = body[fanFSIDSize:] // skip the filesystem identifier

		handleLen := int(binary.NativeEndian.Uint32(body[fanHandleBytes:]))
		handleType := int32(binary.NativeEndian.Uint32(body[fanHandleType:]))
		if fanHandleData+handleLen > len(body) {
			continue
		}
		handle := body[fanHandleData : fanHandleData+handleLen]

		// The name, when present, follows the handle as a NUL-terminated
		// string.
		if rest := body[fanHandleData+handleLen:]; len(rest) > 0 {
			if end := bytes.IndexByte(rest, 0); end >= 0 {
				name = string(rest[:end])
			} else {
				name = string(rest)
			}
		}

		if matched = b.watchesForHandle(handleType, handle); len(matched) > 0 {
			return matched, name, true
		}
	}
	return nil, "", false
}

// watchesForHandle finds every watch whose marked directory a handle refers to.
//
// There can be more than one: a watch on a file and a watch on its parent are
// two watches sharing a single mark, and a change to that file is news to
// both.
//
// The comparison is linear in the number of marks, which is acceptable because
// fanotify exists precisely to need very few — one mark can cover a whole
// mount where inotify would need thousands.
func (b *fanotifyBackend) watchesForHandle(handleType int32, handle []byte) []*fanWatch {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []*fanWatch
	for _, w := range b.watches {
		if w.handleType == handleType && bytes.Equal(w.handle, handle) {
			out = append(out, w)
		}
	}
	return out
}

// fanotifyMask translates the requested operations into a kernel mask.
func fanotifyMask(ops Op) uint64 {
	// Events are wanted for the contents of the marked directory, and for
	// directories as well as files.
	mask := uint64(unix.FAN_EVENT_ON_CHILD | unix.FAN_ONDIR)

	if ops.Has(Create) {
		mask |= unix.FAN_CREATE | unix.FAN_MOVED_TO
	}
	if ops.Has(Write) {
		mask |= unix.FAN_MODIFY
	}
	if ops.Has(Remove) {
		mask |= unix.FAN_DELETE | unix.FAN_DELETE_SELF
	}
	if ops.Has(Rename) {
		mask |= unix.FAN_MOVED_FROM | unix.FAN_MOVE_SELF
	}
	if ops.Has(Chmod) {
		mask |= unix.FAN_ATTRIB
	}
	if ops.Has(UnportableOpen) {
		mask |= unix.FAN_OPEN
	}
	if ops.Has(UnportableRead) {
		mask |= unix.FAN_ACCESS
	}
	if ops.Has(UnportableCloseWrite) {
		mask |= unix.FAN_CLOSE_WRITE
	}
	if ops.Has(UnportableCloseRead) {
		mask |= unix.FAN_CLOSE_NOWRITE
	}
	return mask
}

// fanotifyOps translates a kernel mask into the operations it represents.
func fanotifyOps(mask uint64) Op {
	var op Op

	if mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO) != 0 {
		op |= Create
	}
	if mask&unix.FAN_MODIFY != 0 {
		op |= Write
	}
	if mask&(unix.FAN_DELETE|unix.FAN_DELETE_SELF) != 0 {
		op |= Remove
	}
	if mask&(unix.FAN_MOVED_FROM|unix.FAN_MOVE_SELF) != 0 {
		op |= Rename
	}
	if mask&unix.FAN_ATTRIB != 0 {
		op |= Chmod
	}
	if mask&unix.FAN_OPEN != 0 {
		op |= UnportableOpen
	}
	if mask&unix.FAN_ACCESS != 0 {
		op |= UnportableRead
	}
	if mask&unix.FAN_CLOSE_WRITE != 0 {
		op |= UnportableCloseWrite
	}
	if mask&unix.FAN_CLOSE_NOWRITE != 0 {
		op |= UnportableCloseRead
	}
	return op
}

func fanotifyInitError(err error) error {
	switch {
	case errors.Is(err, unix.EPERM):
		return fmt.Errorf("%w: fanotify requires CAP_SYS_ADMIN", err)
	case errors.Is(err, unix.EINVAL):
		return fmt.Errorf("%w: this kernel does not support reporting changes by "+
			"directory handle and name, which needs Linux 5.9 or newer", err)
	case errors.Is(err, unix.ENOSYS):
		return fmt.Errorf("%w: this kernel was built without fanotify", err)
	default:
		return err
	}
}

func fanotifyMarkError(path string, err error) error {
	switch {
	case errors.Is(err, unix.ENOSPC):
		return fmt.Errorf("%w: the per-user limit on fanotify marks is exhausted; "+
			"raise fs.fanotify.max_user_marks", err)
	case errors.Is(err, unix.ENODEV), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %s is on a filesystem that cannot report changes by "+
			"file handle", err, path)
	default:
		return &os.PathError{Op: "fanotify_mark", Path: path, Err: err}
	}
}

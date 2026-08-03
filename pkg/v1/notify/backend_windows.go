//go:build windows

package notify

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// The Windows backend reads change notifications through ReadDirectoryChangesW
// and an I/O completion port.
//
// Three properties shape the implementation.
//
// It watches directories, never files. A watch on a single file is served by
// watching its parent and discarding everything else, which is what the
// interface offers rather than a shortcut taken here.
//
// It watches subtrees natively, so recursion costs one handle for a whole tree
// rather than one per directory. This backend therefore needs no help from the
// recursive wrapper.
//
// Completion packets outlive the operations that produced them. A packet
// already queued when a watch is removed is still delivered afterwards, and
// Windows reissues both handle values and memory to the next caller. Anything
// that identifies a watch by a value the kernel may reuse will eventually
// attribute an event to the wrong path — or worse, act on a handle that now
// belongs to someone else entirely, including the Go runtime. The design below
// avoids that by construction rather than by care.

// wsBufferSize is the default per-watch buffer for change records.
//
// A burst that exceeds it is discarded by the kernel, which reports the
// overflow by returning no bytes at all. [WithBufferSize] raises it for
// directories that change in large bursts.
const wsBufferSize = 64 * 1024

// wsShutdownKey is the completion key used to wake the reader at shutdown.
//
// Zero is safe to reserve because watch identifiers start at one.
const wsShutdownKey = 0

// Layout of FILE_NOTIFY_INFORMATION, whose fields are read directly from the
// buffer rather than through a struct, so that no unsafe conversion is needed.
const (
	wsOffsetNextEntry = 0
	wsOffsetAction    = 4
	wsOffsetNameLen   = 8
	wsOffsetName      = 12
)

func init() {
	// CapRecursive is deliberately not claimed, even though ReadDirectoryChangesW
	// watches subtrees natively and doing so would cost one handle for a whole
	// tree instead of one per directory.
	//
	// Native subtree watching has the same blind spot as every other interface:
	// a populated directory renamed into the tree is reported as one directory
	// appearing, with nothing said about its contents, which existed before the
	// watch could cover them. It also double-reports a file covered by two
	// overlapping subtree watches, since each handle notices it independently.
	// The shared recursive wrapper already solves both, and is tested. Claiming
	// native recursion to save handles would trade a correctness property for a
	// resource one.
	factoryCaps[BackendDirectoryChanges] = CapPreciseRename

	register(backendFactory{
		kind:     BackendDirectoryChanges,
		priority: 100,
		new:      newWindowsBackend,
	})
}

// wsWatch is one directory being watched.
//
// The overlapped structure and the buffer live here because the kernel writes
// to both asynchronously. Neither may be freed while an operation is in
// flight, which is why a removed watch is retained until its final completion
// arrives rather than being dropped immediately.
type wsWatch struct {
	id     uint64
	handle windows.Handle
	path   string
	opts   addOpts

	// filter, when set, is the single file name this watch cares about. It is
	// how a watch on a file is served by watching its parent directory.
	filter string

	ov      windows.Overlapped
	buf     []byte
	mask    uint32
	subtree bool
}

type windowsBackend struct {
	sink sink
	port windows.Handle

	mu      sync.Mutex
	nextID  uint64
	watches map[uint64]*wsWatch
	byPath  map[string]*wsWatch
	// retiring holds watches whose handles have been closed but whose final
	// completion packet has not yet arrived. They are kept reachable so that
	// the kernel cannot write into memory this process has released.
	retiring map[uint64]*wsWatch
	closed   bool

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func newWindowsBackend(s sink, _ config) (backend, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("creating completion port: %w", err)
	}

	b := &windowsBackend{
		sink:     s,
		port:     port,
		watches:  make(map[uint64]*wsWatch),
		byPath:   make(map[string]*wsWatch),
		retiring: make(map[uint64]*wsWatch),
	}

	b.wg.Add(1)
	go b.read()
	return b, nil
}

func (b *windowsBackend) Kind() Backend { return BackendDirectoryChanges }

func (b *windowsBackend) Capabilities() Capability {
	return factoryCaps[BackendDirectoryChanges]
}

func (b *windowsBackend) Add(path string, opts addOpts) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	// Only a directory can be watched, so watching a file means watching its
	// parent and reporting only that name.
	dir, filter := path, ""
	if !info.IsDir() {
		dir, filter = filepath.Dir(path), filepath.Base(path)
	}

	pathPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("converting path: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	if existing := b.byPath[path]; existing != nil {
		// Re-adding replaces the options. The mask cannot be changed on an
		// operation already in flight, so the old watch is retired and a new
		// one takes its place.
		b.retireLocked(existing)
	}

	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_LIST_DIRECTORY,
		// Sharing deletion matters: without it, watching a directory would
		// prevent anyone from removing or renaming it, so the act of observing
		// would change what is observed.
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return &os.PathError{Op: "CreateFile", Path: dir, Err: err}
	}

	b.nextID++
	w := &wsWatch{
		id:     b.nextID,
		handle: handle,
		path:   path,
		opts:   opts,
		filter: filter,
		buf:    make([]byte, max(opts.bufferSize, wsBufferSize)),
		mask:   windowsMask(opts.ops),
		// Never a subtree watch. Recursion is provided by the wrapper, which
		// adds one watch per directory; leaving this set would make each of
		// those cover the whole tree beneath it, so a file in a nested
		// directory would be reported once per ancestor.
		subtree: false,
	}

	// The completion key is our own identifier, never a handle or a pointer.
	// Identifiers are not reused, so a packet belonging to a removed watch
	// resolves to nothing and is discarded — which is the whole defence
	// against acting on a value the kernel has since given to someone else.
	if _, err := windows.CreateIoCompletionPort(handle, b.port, uintptr(w.id), 0); err != nil {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("associating %s with the completion port: %w", dir, err)
	}

	if err := b.armLocked(w); err != nil {
		_ = windows.CloseHandle(handle)
		return err
	}

	b.watches[w.id] = w
	b.byPath[path] = w
	return nil
}

// armLocked issues a read for the next batch of changes. b.mu must be held.
func (b *windowsBackend) armLocked(w *wsWatch) error {
	err := windows.ReadDirectoryChanges(
		w.handle,
		&w.buf[0],
		uint32(len(w.buf)),
		w.subtree,
		w.mask,
		nil, // asynchronous: the length arrives with the completion packet
		&w.ov,
		0,
	)
	if err != nil {
		return fmt.Errorf("watching %s: %w", w.path, err)
	}
	return nil
}

// retireLocked closes a watch's handle and keeps it reachable until its final
// completion packet arrives. b.mu must be held.
func (b *windowsBackend) retireLocked(w *wsWatch) {
	delete(b.watches, w.id)
	delete(b.byPath, w.path)

	// Cancel first so that the pending operation completes promptly, then
	// close. The watch is not dropped here: the kernel may still write to its
	// overlapped structure, and releasing that memory now would corrupt
	// whatever the allocator hands out next.
	_ = windows.CancelIoEx(w.handle, &w.ov)
	_ = windows.CloseHandle(w.handle)
	b.retiring[w.id] = w
}

func (b *windowsBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	w := b.byPath[path]
	if w == nil {
		return ErrNonExistentWatch
	}
	b.retireLocked(w)
	return nil
}

func (b *windowsBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.byPath))
	for path := range b.byPath {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (b *windowsBackend) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		for _, w := range b.watches {
			b.retireLocked(w)
		}
		b.mu.Unlock()

		// Wake the reader. Cancelled operations produce completion packets of
		// their own, but there may be none pending at all, so shutdown cannot
		// rely on them arriving.
		if err := windows.PostQueuedCompletionStatus(b.port, 0, wsShutdownKey, nil); err != nil {
			b.closeErr = fmt.Errorf("signalling shutdown: %w", err)
		}

		b.wg.Wait()
		_ = windows.CloseHandle(b.port)
	})
	return b.closeErr
}

func (b *windowsBackend) read() {
	defer b.wg.Done()

	for {
		var (
			transferred uint32
			key         uintptr
			ov          *windows.Overlapped
		)
		err := windows.GetQueuedCompletionStatus(b.port, &transferred, &key, &ov, windows.INFINITE)

		if key == wsShutdownKey {
			return
		}

		b.mu.Lock()
		w := b.watches[uint64(key)]
		if w == nil {
			// A packet for a watch that has been removed. Now that it has
			// arrived, the kernel is finished with the watch's memory and it
			// can be released.
			delete(b.retiring, uint64(key))
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		b.mu.Unlock()

		if err != nil {
			// The operation was cancelled because the watch is going away;
			// that is not a failure worth reporting.
			if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
				continue
			}
			// A watched directory that has been deleted or renamed away leaves
			// its handle referring to something no longer reachable by name.
			// The kernel reports that as an access failure rather than as an
			// event, so the watch's own disappearance has to be recognised
			// here or it would never be reported at all.
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
				b.mu.Lock()
				live := b.watches[w.id] == w
				if live {
					b.retireLocked(w)
				}
				b.mu.Unlock()
				if live && w.opts.ops.Has(Remove) && !b.sink.send(Event{Name: w.path, Op: Remove}) {
					return
				}
				continue
			}
			if !b.sink.fail(fmt.Errorf("notify: watching %s: %w", w.path, err)) {
				return
			}
			continue
		}

		// No bytes means the buffer was too small for the burst and the kernel
		// discarded the changes rather than truncating them. Watching
		// continues, but the caller's picture of the tree is now incomplete.
		if transferred == 0 {
			if !b.sink.fail(fmt.Errorf("notify: %w: changes to %s were discarded because the "+
				"buffer filled; raise it with WithBufferSize", ErrEventOverflow, w.path)) {
				return
			}
		} else if !b.dispatch(w, w.buf[:transferred]) {
			return
		}

		// Re-arm only after confirming the watch is still live. A watch
		// removed while this packet was in flight must not be revived, and its
		// handle must not be touched: the value has already been closed and
		// Windows may have given it to another part of this program.
		b.mu.Lock()
		if b.watches[w.id] == w && !b.closed {
			if err := b.armLocked(w); err != nil {
				// The watch cannot be continued, so retire it rather than
				// leaving an entry that will never report anything again.
				b.retireLocked(w)
			}
		}
		b.mu.Unlock()
	}
}

// dispatch decodes a batch of change records and delivers them.
func (b *windowsBackend) dispatch(w *wsWatch, buf []byte) bool {
	for {
		if len(buf) < wsOffsetName {
			return true
		}

		next := binary.NativeEndian.Uint32(buf[wsOffsetNextEntry:])
		action := binary.NativeEndian.Uint32(buf[wsOffsetAction:])
		nameLen := binary.NativeEndian.Uint32(buf[wsOffsetNameLen:])

		if uint64(wsOffsetName)+uint64(nameLen) > uint64(len(buf)) {
			b.sink.fail(fmt.Errorf("notify: truncated change record for %s", w.path))
			return true
		}

		name := decodeUTF16(buf[wsOffsetName : wsOffsetName+int(nameLen)])
		if !b.emit(w, action, name) {
			return false
		}

		if next == 0 {
			return true
		}
		if uint64(next) >= uint64(len(buf)) {
			return true
		}
		buf = buf[next:]
	}
}

// emit turns one change record into an event, if the watch wants it.
func (b *windowsBackend) emit(w *wsWatch, action uint32, name string) bool {
	if debugEnabled {
		debugf("windows action=%d name=%q watch=%s", action, name, w.path)
	}

	// A watch on a single file sees everything happening in its parent, so
	// everything else is discarded here.
	if w.filter != "" && !strings.EqualFold(name, w.filter) {
		return true
	}

	op := windowsOp(action, w.opts.ops)
	if op == 0 {
		return true
	}

	path := filepath.Join(filepath.Dir(w.path), name)
	if w.filter == "" {
		path = filepath.Join(w.path, name)
	}
	return b.sink.send(Event{Name: path, Op: op})
}

// windowsMask translates the requested operations into a notify filter.
func windowsMask(ops Op) uint32 {
	var mask uint32
	if ops.Has(Create) || ops.Has(Remove) || ops.Has(Rename) {
		mask |= windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME
	}
	if ops.Has(Write) {
		mask |= windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_SIZE
	}
	if ops.Has(Chmod) {
		mask |= windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SECURITY
	}
	return mask
}

// windowsOp translates a change action into the operations it represents.
//
// Windows reports a single "modified" action for a change to contents,
// attributes, or security, so a write and a permission change are
// indistinguishable once they arrive. The filter requested at registration is
// the only evidence available, so a watch interested in writes reads the
// action as a write, and one interested only in attributes reads it as a
// metadata change.
func windowsOp(action uint32, ops Op) Op {
	var op Op
	switch action {
	case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_RENAMED_NEW_NAME:
		// A file moved into a watched directory is, from the directory's point
		// of view, a new entry appearing.
		op = Create
	case windows.FILE_ACTION_REMOVED:
		op = Remove
	case windows.FILE_ACTION_RENAMED_OLD_NAME:
		op = Rename
	case windows.FILE_ACTION_MODIFIED:
		if ops.Has(Write) {
			op = Write
		} else {
			op = Chmod
		}
	default:
		return 0
	}
	return op & ops
}

// decodeUTF16 converts a UTF-16 name from the change record.
//
// The bytes are decoded a unit at a time rather than reinterpreted as a
// wider type, which keeps this free of unsafe and correct regardless of the
// buffer's alignment.
func decodeUTF16(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.NativeEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(units))
}

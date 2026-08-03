//go:build illumos || solaris

package notify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The FEN backend watches files through an illumos event port.
//
// Two properties of the interface shape everything here, and neither has an
// equivalent in the other backends.
//
// Associations are one-shot. A path is registered, one event is delivered, and
// the registration is then gone — not disabled, gone. Every event must be
// followed by re-registering the path or the watch silently stops after its
// first notification, which is the defining mistake to make with this
// interface and produces a watcher that appears to work exactly once.
//
// Re-registering is also the only way to notice that a file has been replaced,
// because an association is made against a path *and* the timestamps that path
// had at the time. If the file changed between the event and the
// re-registration, the kernel says so rather than silently watching the new
// one.
//
// Like kqueue, an event on a directory says only that it changed, so listings
// are compared to discover which name appeared or vanished.

// fenEventBufferSize is how many events are collected per call. Batching
// matters because a directory hit repeatedly is then listed once.
const fenEventBufferSize = 64

// fenWakeInterval bounds how long shutdown waits for the reader.
//
// The port has no side channel to interrupt a blocked wait — no descriptor a
// pipe could be attached to — so the reader wakes periodically and checks
// instead. The interval trades a negligible amount of idle work for a bounded
// shutdown.
const fenWakeInterval = 250 * time.Millisecond

// fenWatchedEvents is what an association asks for.
//
// FILE_NOFOLLOW is added per watch when the caller asked not to follow
// symlinks; it is not included here because most watches want the target.
const fenWatchedEvents = unix.FILE_MODIFIED | unix.FILE_ATTRIB | unix.FILE_TRUNC

func init() {
	factoryCaps[BackendFEN] = CapNoFollow | CapPreciseRename

	register(backendFactory{
		kind:      BackendFEN,
		priority:  100,
		available: fenAvailable,
		new:       newFENBackend,
	})
}

func fenAvailable() bool {
	port, err := unix.NewEventPort()
	if err != nil {
		return false
	}
	_ = port.Close()
	return true
}

// fenWatch is one associated path.
type fenWatch struct {
	path  string
	isDir bool
	opts  addOpts

	// explicit distinguishes a path the caller asked for from one associated
	// because it sits inside a watched directory. As with kqueue, exactly one
	// of a child and its parent may report the child's disappearance, and the
	// parent is chosen because only it can tell a removal from a rename.
	explicit bool

	// entries is the directory's last known contents, which is the only way to
	// discover what an event on a directory actually referred to.
	entries map[string]fileID
}

type fenBackend struct {
	sink sink

	port *unix.EventPort

	mu      sync.Mutex
	watches map[string]*fenWatch
	closed  bool

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func newFENBackend(s sink, _ config) (backend, error) {
	port, err := unix.NewEventPort()
	if err != nil {
		return nil, fmt.Errorf("creating event port: %w", err)
	}

	b := &fenBackend{
		sink:    s,
		port:    port,
		watches: make(map[string]*fenWatch),
	}

	b.wg.Add(1)
	go b.read()
	return b, nil
}

func (b *fenBackend) Kind() Backend { return BackendFEN }

func (b *fenBackend) Capabilities() Capability { return factoryCaps[BackendFEN] }

func (b *fenBackend) Add(path string, opts addOpts) error {
	info, err := statPath(path, opts.noFollow)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	if existing := b.watches[path]; existing != nil {
		existing.opts = opts
		existing.explicit = true
		return nil
	}

	w := &fenWatch{path: path, isDir: info.IsDir(), explicit: true, opts: opts}
	if w.isDir {
		w.entries = make(map[string]fileID)
	}
	if err := b.associateLocked(w, info); err != nil {
		return err
	}
	b.watches[path] = w

	if w.isDir {
		b.rescanLocked(w, false)
	}
	return nil
}

// associateLocked registers a path with the port. b.mu must be held.
//
// The association carries the file's current timestamps, so the kernel can
// tell us if the path stops referring to the same file. Passing a stale
// [os.FileInfo] would defeat that, which is why the caller supplies a fresh
// one rather than this reusing whatever it saw last.
func (b *fenBackend) associateLocked(w *fenWatch, info os.FileInfo) error {
	events := fenWatchedEvents
	if w.opts.noFollow {
		events |= unix.FILE_NOFOLLOW
	}

	if err := b.port.AssociatePath(w.path, info, events, nil); err != nil {
		return fenAssociateError(w.path, err)
	}
	return nil
}

// rearmLocked re-registers a path after an event consumed its association.
// b.mu must be held.
//
// It reports whether the path is still watchable. A path that has gone cannot
// be re-registered, and that is how this backend learns a watched file
// disappeared: the failure is the notification.
func (b *fenBackend) rearmLocked(w *fenWatch) bool {
	info, err := statPath(w.path, w.opts.noFollow)
	if err != nil {
		return false
	}
	if err := b.associateLocked(w, info); err != nil {
		debugf("re-associating %s: %s", w.path, err)
		return false
	}
	return true
}

func (b *fenBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	w := b.watches[path]
	if w == nil || !w.explicit {
		return ErrNonExistentWatch
	}
	b.dropTreeLocked(w)
	return nil
}

// dropLocked releases one association. b.mu must be held.
func (b *fenBackend) dropLocked(w *fenWatch) {
	delete(b.watches, w.path)

	// A path whose association has already been consumed by an event, or whose
	// file is gone, is not registered any more. Asking to remove it then is
	// not a failure.
	if err := b.port.DissociatePath(w.path); err != nil &&
		!errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
		debugf("dissociating %s: %s", w.path, err)
	}
}

func (b *fenBackend) dropTreeLocked(w *fenWatch) {
	if w.isDir {
		for _, child := range b.childrenLocked(w.path) {
			b.dropLocked(child)
		}
	}
	b.dropLocked(w)
}

func (b *fenBackend) childrenLocked(dir string) []*fenWatch {
	var out []*fenWatch
	for path, w := range b.watches {
		if w.explicit || filepath.Dir(path) != dir {
			continue
		}
		out = append(out, w)
	}
	return out
}

func (b *fenBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.watches))
	for path, w := range b.watches {
		if w.explicit {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func (b *fenBackend) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()

		// The reader notices within one wake interval; there is no descriptor
		// to interrupt it through.
		b.wg.Wait()

		b.mu.Lock()
		for _, w := range b.watches {
			_ = b.port.DissociatePath(w.path)
		}
		b.watches = map[string]*fenWatch{}
		b.mu.Unlock()

		if err := b.port.Close(); err != nil {
			b.closeErr = fmt.Errorf("closing event port: %w", err)
		}
	})
	return b.closeErr
}

func (b *fenBackend) read() {
	defer b.wg.Done()

	events := make([]unix.PortEvent, fenEventBufferSize)
	timeout := unix.NsecToTimespec(int64(fenWakeInterval))

	for {
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if closed || b.sink.closing() {
			return
		}

		n, err := b.port.Get(events, 1, &timeout)
		if err != nil {
			// A timeout means only that nothing happened, which is the common
			// case for an idle watcher.
			if errors.Is(err, unix.ETIME) || errors.Is(err, unix.EINTR) {
				continue
			}
			b.sink.fail(fmt.Errorf("notify: waiting for events: %w", err))
			return
		}
		if n == 0 {
			continue
		}
		if !b.dispatch(events[:n]) {
			return
		}
	}
}

// dispatch handles one batch of port events.
//
// As with kqueue this runs in two passes, because a rename produces an event
// on the file and another on its parent, and the parent cannot interpret its
// own listing without knowing what the child reported. Processing events one
// at a time would make the result depend on the order the kernel delivered
// them.
func (b *fenBackend) dispatch(events []unix.PortEvent) bool {
	type report struct {
		op      Op
		renamed bool
		gone    bool
	}

	var (
		changes  = make(map[string]*report)
		rescan   = make(map[string]bool)
		selfGone []*fenWatch
	)

	b.mu.Lock()
	for i := range events {
		ev := &events[i]
		if ev.Source != unix.PORT_SOURCE_FILE {
			continue
		}

		path := ev.Path
		w := b.watches[path]
		if w == nil {
			continue // removed since this event was queued
		}
		mask := uint32(ev.Events)

		if debugEnabled {
			debugf("fen path=%s events=%s", path, fenEventsString(mask))
		}

		r := changes[path]
		if r == nil {
			r = &report{}
			changes[path] = r
		}

		switch {
		case w.isDir:
			if mask&(unix.FILE_MODIFIED|unix.FILE_TRUNC) != 0 {
				rescan[path] = true
			}
			if mask&unix.FILE_ATTRIB != 0 {
				r.op |= Chmod
			}
		default:
			if mask&(unix.FILE_MODIFIED|unix.FILE_TRUNC) != 0 {
				r.op |= Write
			}
			if mask&unix.FILE_ATTRIB != 0 {
				r.op |= Chmod
			}
		}

		if mask&unix.FILE_RENAME_FROM != 0 {
			r.renamed = true
		}
		if mask&(unix.FILE_DELETE|unix.FILE_RENAME_TO) != 0 {
			r.gone = true
		}

		// An association is consumed by the event that used it. Re-register,
		// and treat failing to do so as the path having gone — which is
		// exactly what it means.
		if r.renamed || r.gone || !b.rearmLocked(w) {
			r.gone = r.gone || !r.renamed
			selfGone = append(selfGone, w)
			if parent := b.watches[filepath.Dir(path)]; parent != nil && parent.isDir {
				rescan[parent.path] = true
			}
		}
	}
	b.mu.Unlock()

	for _, path := range sortedKeys(changes) {
		r := changes[path]
		if r.op == 0 {
			continue
		}
		b.mu.Lock()
		w := b.watches[path]
		b.mu.Unlock()
		if w == nil {
			continue
		}
		if op := r.op & w.opts.ops; op != 0 && !b.sink.send(Event{Name: path, Op: op}) {
			return false
		}
	}

	for _, dir := range sortedKeys(rescan) {
		b.mu.Lock()
		w := b.watches[dir]
		b.mu.Unlock()
		if w == nil || !w.isDir {
			continue
		}

		b.mu.Lock()
		evs := b.rescanLocked(w, true)
		opts := w.opts
		b.mu.Unlock()

		for _, ev := range evs {
			if ev.Op.Has(Remove) {
				if r := changes[ev.Name]; r != nil && r.renamed && !r.gone {
					ev.Op = ev.Op&^Remove | Rename
				}
			}
			if ev.Op &= opts.ops; ev.Op == 0 {
				continue
			}
			if !b.sink.send(ev) {
				return false
			}
		}
	}

	for _, w := range selfGone {
		r := changes[w.path]

		b.mu.Lock()
		still := b.watches[w.path] == w
		if still {
			b.dropTreeLocked(w)
		}
		b.mu.Unlock()
		if !still || !w.explicit {
			continue // a child is reported by its parent's listing
		}

		op := Remove
		if r != nil && r.renamed && !r.gone {
			op = Rename
		}
		if op &= w.opts.ops; op == 0 {
			continue
		}
		if !b.sink.send(Event{Name: w.path, Op: op}) {
			return false
		}
	}
	return true
}

// rescanLocked refreshes a directory's listing and associates any new entries.
// b.mu must be held.
func (b *fenBackend) rescanLocked(w *fenWatch, report bool) []Event {
	entries, err := os.ReadDir(w.path)
	if err != nil {
		return nil
	}

	current := make(map[string]fileID, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		id, _ := statID(info)
		current[entry.Name()] = id
	}

	var events []Event
	if report {
		events = diffEntries(w.path, w.entries, current)
	}
	w.entries = current

	if needsChildWatches(w.opts) {
		b.syncChildrenLocked(w, current)
	}
	return events
}

// syncChildrenLocked associates entries that need watching in their own right
// and releases those that have gone. b.mu must be held.
func (b *fenBackend) syncChildrenLocked(w *fenWatch, current map[string]fileID) {
	for name := range current {
		path := filepath.Join(w.path, name)
		if b.watches[path] != nil {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			// A subdirectory's contents are a recursive watch's business; a
			// plain directory watch reports only that it exists.
			continue
		}

		child := &fenWatch{path: path, opts: w.opts}
		if err := b.associateLocked(child, info); err != nil {
			// A dangling symlink or an unreadable file is not a reason to stop
			// watching the rest of the directory.
			debugf("not watching %s: %s", path, err)
			continue
		}
		b.watches[path] = child
	}

	for _, child := range b.childrenLocked(w.path) {
		if _, ok := current[filepath.Base(child.path)]; !ok {
			b.dropLocked(child)
		}
	}
}

func fenAssociateError(path string, err error) error {
	if errors.Is(err, unix.ENOMEM) || errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("%w: the event port is out of resources; watch fewer paths", err)
	}
	return &os.PathError{Op: "port_associate", Path: path, Err: err}
}

func fenEventsString(mask uint32) string {
	bits := []struct {
		bit  uint32
		name string
	}{
		{unix.FILE_MODIFIED, "MODIFIED"},
		{unix.FILE_ATTRIB, "ATTRIB"},
		{unix.FILE_TRUNC, "TRUNC"},
		{unix.FILE_DELETE, "DELETE"},
		{unix.FILE_RENAME_TO, "RENAME_TO"},
		{unix.FILE_RENAME_FROM, "RENAME_FROM"},
		{unix.UNMOUNTED, "UNMOUNTED"},
		{unix.MOUNTEDOVER, "MOUNTEDOVER"},
	}

	var out []string
	for _, x := range bits {
		if mask&x.bit != 0 {
			out = append(out, x.name)
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("0x%x", mask)
	}
	return joinStrings(out, "|")
}

//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package notify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
)

// The kqueue backend watches files and directories through the BSD kernel
// queue.
//
// Three properties of kqueue shape this implementation, and all three are
// consequences of the interface rather than choices made here.
//
// It reports changes per *open file descriptor*. Watching anything means
// holding a descriptor open for it, so the number of watched paths competes
// directly with the process's own limit on open files. This is the reason the
// descriptor cost is made avoidable wherever possible: a watch that does not
// need per-file events does not open per-file descriptors.
//
// It does not say what changed in a directory. A directory reports only that
// its contents were modified, so the entries must be listed and compared
// against the previous listing to discover which name appeared or vanished.
//
// It does not pair a rename. A file renamed within a directory looks exactly
// like one file vanishing and another appearing. The two are distinguished
// here by asking the vanished file's own descriptor whether it was renamed or
// deleted, and by matching file identity across the two names.

// kqEventBufferSize is how many kernel events are collected per call.
//
// Larger batches matter more than they might appear: a batch is processed as a
// unit, and directory rescans are deduplicated within one, so a burst of
// changes in a single directory costs one listing rather than one per event.
const kqEventBufferSize = 64

// fflags for a watched file. NOTE_EXTEND accompanies NOTE_WRITE when a file
// grows, and NOTE_REVOKE fires when the filesystem beneath it is forcibly
// unmounted.
const kqFileFlags = unix.NOTE_DELETE | unix.NOTE_WRITE | unix.NOTE_EXTEND |
	unix.NOTE_ATTRIB | unix.NOTE_RENAME | unix.NOTE_REVOKE

// fflags for a watched directory. NOTE_LINK reports a change to the link
// count, which is how the creation or removal of a subdirectory appears.
const kqDirFlags = unix.NOTE_DELETE | unix.NOTE_WRITE | unix.NOTE_ATTRIB |
	unix.NOTE_RENAME | unix.NOTE_LINK | unix.NOTE_REVOKE

func init() {
	factoryCaps[BackendKqueue] = CapNoFollow | CapPreciseRename | CapFDPerPath

	register(backendFactory{
		kind:      BackendKqueue,
		priority:  100,
		available: kqueueAvailable,
		new:       newKqueueBackend,
	})
}

// errFDBudgetExhausted is returned internally when an optional descriptor
// cannot be afforded. It never reaches a caller: seeing it, this backend
// declines to watch that individual file and carries on, which costs
// modification events for that file and nothing else.
var errFDBudgetExhausted = fmt.Errorf("%w: descriptor budget exhausted", ErrUnsupported)

func kqueueAvailable() bool {
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	_ = unix.Close(kq)
	return true
}

// kqWatch is one open descriptor being watched.
type kqWatch struct {
	fd    int
	path  string
	isDir bool
	opts  addOpts

	// explicit distinguishes a path the caller asked for from one opened
	// because it happens to sit inside a watched directory.
	//
	// The distinction decides who reports a disappearance. A child's own
	// descriptor and its parent's listing both notice the same removal, so
	// exactly one of them must speak: the parent, which is the only one that
	// can tell a rename from a delete by comparing listings.
	explicit bool

	// entries is the last known contents of a directory, by name. Directories
	// report only that something changed, so this is what the change is
	// measured against.
	entries map[string]fileID
}

type kqueueBackend struct {
	sink sink

	kq   int
	wake [2]int

	// budget bounds how many descriptors watching may hold, so that a watcher
	// on a large tree cannot starve the program it belongs to.
	budget *fdBudget

	// degraded compares the files the budget refused a descriptor, so that
	// running out costs precision and latency rather than silence.
	degraded *degradedPoller

	mu     sync.Mutex
	byFD   map[int]*kqWatch
	byPath map[string]*kqWatch
	closed bool

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func newKqueueBackend(s sink, cfg config) (backend, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("creating kernel queue: %w", err)
	}
	// Kqueue() does not accept flags, so close-on-exec is set separately. A
	// descriptor leaked into a child process would keep watches alive after
	// this process has finished with them.
	if _, err := unix.FcntlInt(uintptr(kq), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("setting close-on-exec on kernel queue: %w", err)
	}

	b := &kqueueBackend{
		sink:     s,
		kq:       kq,
		budget:   newFDBudget(cfg.fdBudget),
		degraded: newDegradedPoller(s, cfg.pollInterval),
		byFD:     make(map[int]*kqWatch),
		byPath:   make(map[string]*kqWatch),
	}

	if err := newWakePipe(&b.wake); err != nil {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("creating shutdown pipe: %w", err)
	}
	if err := b.registerRead(b.wake[0]); err != nil {
		_ = unix.Close(kq)
		_ = unix.Close(b.wake[0])
		_ = unix.Close(b.wake[1])
		return nil, fmt.Errorf("registering shutdown pipe: %w", err)
	}

	b.wg.Add(1)
	go b.read()
	return b, nil
}

func (b *kqueueBackend) Kind() Backend { return BackendKqueue }

// budgetStats reports descriptor accounting for Watcher.Stats.
func (b *kqueueBackend) budgetStats() (held, budget, denied, limit int) {
	return b.budget.snapshot()
}

func (b *kqueueBackend) Capabilities() Capability { return factoryCaps[BackendKqueue] }

// registerVnode arms a descriptor for change notifications.
//
// EV_CLEAR makes the registration edge-triggered: each change is reported once
// rather than continuously until acknowledged.
func (b *kqueueBackend) registerVnode(fd int, fflags uint32) error {
	var kev unix.Kevent_t
	unix.SetKevent(&kev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR|unix.EV_ENABLE)
	kev.Fflags = fflags

	// A nil events slice makes this call register and return rather than wait.
	_, err := unix.Kevent(b.kq, []unix.Kevent_t{kev}, nil, nil)
	return err
}

func (b *kqueueBackend) registerRead(fd int) error {
	var kev unix.Kevent_t
	unix.SetKevent(&kev, fd, unix.EVFILT_READ, unix.EV_ADD|unix.EV_ENABLE)
	_, err := unix.Kevent(b.kq, []unix.Kevent_t{kev}, nil, nil)
	return err
}

// openForWatch opens a path solely to watch it.
//
// The flags matter. Opening for events only, where the platform offers it,
// avoids holding a reference that would block an unmount. Non-blocking is
// required because opening a named pipe for reading otherwise waits for a
// writer, which would hang the caller on a path that merely happens to be a
// fifo.
func openForWatch(path string, noFollow bool) (int, error) {
	flags := kqOpenFlags
	if noFollow {
		flags |= unix.O_NOFOLLOW
	}
	return unix.Open(path, flags, 0)
}

func (b *kqueueBackend) Add(path string, opts addOpts) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	// Lstat describes the link itself; without WithNoFollow the watch belongs
	// on whatever it points at.
	if !opts.noFollow && info.Mode()&os.ModeSymlink != 0 {
		if info, err = os.Stat(path); err != nil {
			return err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	if existing := b.byPath[path]; existing != nil {
		// Re-adding an existing watch updates its options rather than opening
		// a second descriptor for the same file.
		existing.opts = opts
		existing.explicit = true
		return nil
	}

	w, err := b.watchLocked(path, info.IsDir(), true, opts)
	if err != nil {
		return err
	}
	if w.isDir {
		b.rescanLocked(w, false)
	}
	return nil
}

// watchLocked opens and arms a path. b.mu must be held.
func (b *kqueueBackend) watchLocked(path string, isDir, explicit bool, opts addOpts) (*kqWatch, error) {
	// A path the caller asked for is always watched, even if that exceeds the
	// budget: refusing an explicit Add because of an internal accounting limit
	// would be a worse failure than the one the budget exists to prevent. The
	// budget governs the descriptors this backend opens on its own initiative
	// — those for the individual files inside a watched directory — which are
	// exactly the ones it can do without.
	if explicit {
		b.budget.reserve()
	} else if !b.budget.acquire() {
		return nil, errFDBudgetExhausted
	}

	fd, err := openForWatch(path, opts.noFollow)
	if err != nil {
		b.budget.release()
		return nil, kqOpenError(path, err)
	}

	fflags := uint32(kqFileFlags)
	if isDir {
		fflags = uint32(kqDirFlags)
	}
	if err := b.registerVnode(fd, fflags); err != nil {
		_ = unix.Close(fd)
		b.budget.release()
		return nil, fmt.Errorf("watching %s: %w", path, err)
	}

	w := &kqWatch{fd: fd, path: path, isDir: isDir, explicit: explicit, opts: opts}
	if isDir {
		w.entries = make(map[string]fileID)
	}
	b.byFD[fd] = w
	b.byPath[path] = w
	return w, nil
}

// dropLocked releases a watch. b.mu must be held.
func (b *kqueueBackend) dropLocked(w *kqWatch) {
	if w == nil {
		return
	}
	delete(b.byFD, w.fd)
	delete(b.byPath, w.path)

	// Deregister before closing, even though closing a descriptor also removes
	// its registration.
	//
	// The reason is what closing does *not* do: events already queued against
	// this descriptor stay in the queue. The kernel then reissues the
	// descriptor number to the next caller — very likely this backend, opening
	// the next path to watch — and a stale event surfaces bearing a number
	// that now means something else entirely, so a change to a file that no
	// longer exists is reported against an unrelated one. EV_DELETE discards
	// the pending events with the registration, closing the window.
	var kev unix.Kevent_t
	unix.SetKevent(&kev, w.fd, unix.EVFILT_VNODE, unix.EV_DELETE)
	if _, err := unix.Kevent(b.kq, []unix.Kevent_t{kev}, nil, nil); err != nil {
		// ENOENT means the kernel dropped the registration already, which is
		// what happens when the file itself is gone. That is the normal case
		// here, not a failure.
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EBADF) {
			debugf("deregistering %s: %s", w.path, err)
		}
	}

	_ = unix.Close(w.fd)
	b.budget.release()
}

// dropTreeLocked releases a watch and any child watches beneath it.
func (b *kqueueBackend) dropTreeLocked(w *kqWatch) {
	if w.isDir {
		for _, child := range b.childrenLocked(w.path) {
			b.dropLocked(child)
		}
	}
	b.dropLocked(w)
}

// childrenLocked returns the non-explicit watches directly inside dir.
func (b *kqueueBackend) childrenLocked(dir string) []*kqWatch {
	var out []*kqWatch
	for path, w := range b.byPath {
		if w.explicit || filepath.Dir(path) != dir {
			continue
		}
		out = append(out, w)
	}
	return out
}

func (b *kqueueBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	w := b.byPath[path]
	if w == nil || !w.explicit {
		return ErrNonExistentWatch
	}
	b.dropTreeLocked(w)
	return nil
}

func (b *kqueueBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.byPath))
	for path, w := range b.byPath {
		if w.explicit {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func (b *kqueueBackend) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()

		// Wake the reader before closing anything it is using: a thread parked
		// in kevent does not reliably notice its queue being closed, and the
		// descriptor number may be reused underneath it.
		if _, err := unix.Write(b.wake[1], []byte{0}); err != nil && !errors.Is(err, unix.EAGAIN) {
			b.closeErr = fmt.Errorf("signalling shutdown: %w", err)
		}

		b.degraded.close()

		b.wg.Wait()

		b.mu.Lock()
		for _, w := range b.byFD {
			_ = unix.Close(w.fd)
		}
		b.byFD = map[int]*kqWatch{}
		b.byPath = map[string]*kqWatch{}
		b.mu.Unlock()

		_ = unix.Close(b.kq)
		_ = unix.Close(b.wake[0])
		_ = unix.Close(b.wake[1])
	})
	return b.closeErr
}

func (b *kqueueBackend) read() {
	defer b.wg.Done()

	events := make([]unix.Kevent_t, kqEventBufferSize)
	for {
		// A nil timeout blocks until something happens.
		n, err := unix.Kevent(b.kq, nil, events, nil)
		if err != nil {
			// The Go runtime preempts goroutines with signals, so an
			// interrupted wait is routine rather than exceptional.
			if errors.Is(err, unix.EINTR) {
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

// kqChange is what one batch of kernel events says about a single path.
type kqChange struct {
	op      Op
	renamed bool
	deleted bool
}

// dispatch processes one batch of kernel events.
//
// It runs in two passes. The first records what happened to each descriptor;
// the second acts on it. The order matters because a rename produces two
// related events — one on the file's own descriptor and one on its parent's —
// and the parent cannot tell a rename from a delete without what the child
// reported. Processing events one at a time would make the interpretation
// depend on the order the kernel happened to deliver them.
//
// Batching also means a directory that received twenty changes is listed once.
func (b *kqueueBackend) dispatch(events []unix.Kevent_t) bool {
	var (
		changes  = make(map[string]*kqChange)
		rescan   = make(map[string]bool)
		selfGone []*kqWatch
	)

	b.mu.Lock()
	for i := range events {
		ev := &events[i]
		fd := int(ev.Ident)

		if int32(ev.Filter) == int32(unix.EVFILT_READ) && fd == b.wake[0] {
			b.mu.Unlock()
			return false // Close asked us to stop
		}
		if int32(ev.Filter) != int32(unix.EVFILT_VNODE) {
			continue
		}

		w := b.byFD[fd]
		if w == nil {
			// A watch removed since this event was queued. Darwin also
			// delivers occasional events with an identifier of zero that
			// correspond to no descriptor at all; both land here and are
			// ignored, which is the only safe response to an event we cannot
			// attribute to a path.
			continue
		}
		fflags := uint32(ev.Fflags)

		if debugEnabled {
			debugf("kqueue fd=%d path=%s flags=%s", fd, w.path, kqFlagsString(fflags))
		}

		change := changes[w.path]
		if change == nil {
			change = &kqChange{}
			changes[w.path] = change
		}

		switch {
		case w.isDir:
			// A directory says only that something inside it changed; the
			// listing has to be compared to find out what.
			if fflags&(unix.NOTE_WRITE|unix.NOTE_LINK) != 0 {
				rescan[w.path] = true
			}
			if fflags&unix.NOTE_ATTRIB != 0 {
				change.op |= Chmod
			}
		default:
			if fflags&(unix.NOTE_WRITE|unix.NOTE_EXTEND) != 0 {
				change.op |= Write
			}
			if fflags&unix.NOTE_ATTRIB != 0 {
				change.op |= Chmod
			}
		}

		// A path that has gone is recorded rather than reported. If it sits
		// inside a watched directory, that directory's listing is the only
		// place a rename can be told from a delete, so it speaks instead.
		if fflags&unix.NOTE_RENAME != 0 {
			change.renamed = true
		}
		if fflags&(unix.NOTE_DELETE|unix.NOTE_REVOKE) != 0 {
			change.deleted = true
		}
		if change.renamed || change.deleted {
			selfGone = append(selfGone, w)
			if parent := b.byPath[filepath.Dir(w.path)]; parent != nil && parent.isDir {
				rescan[parent.path] = true
			}
		}
	}
	b.mu.Unlock()

	// Report per-path changes that need no listing to interpret.
	for _, path := range sortedKeys(changes) {
		change := changes[path]
		if change.op == 0 {
			continue
		}
		b.mu.Lock()
		w := b.byPath[path]
		var ops Op
		if w != nil {
			ops = w.opts.ops
		}
		b.mu.Unlock()
		if w == nil {
			continue
		}
		if op := change.op & ops; op != 0 && !b.sink.send(Event{Name: path, Op: op}) {
			return false
		}
	}

	// Compare listings for directories whose contents moved.
	for _, dir := range sortedKeys(rescan) {
		b.mu.Lock()
		w := b.byPath[dir]
		b.mu.Unlock()
		if w == nil || !w.isDir {
			continue
		}
		if !b.rescan(w, changes) {
			return false
		}
	}

	// Retire watches whose paths are gone, reporting the ones the caller asked
	// for by name.
	for _, w := range selfGone {
		change := changes[w.path]

		b.mu.Lock()
		still := b.byPath[w.path] == w
		if still {
			b.dropTreeLocked(w)
		}
		b.mu.Unlock()
		if !still || !w.explicit {
			// A child inside a watched directory has already been reported by
			// that directory's listing.
			continue
		}

		op := Remove
		if change != nil && change.renamed && !change.deleted {
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

// rescan compares a directory's contents against its previous listing and
// reports the difference.
func (b *kqueueBackend) rescan(w *kqWatch, changes map[string]*kqChange) bool {
	b.mu.Lock()
	events := b.rescanLocked(w, true)
	opts := w.opts
	b.mu.Unlock()

	for _, ev := range events {
		// A vanished entry whose own descriptor reported a rename was moved,
		// not deleted. The descriptor knew; the listing alone could not.
		if ev.Op.Has(Remove) {
			if change := changes[ev.Name]; change != nil && change.renamed && !change.deleted {
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
	return true
}

// rescanLocked refreshes a directory's listing, arming watches on new entries
// and releasing them for entries that have gone. b.mu must be held.
//
// With report false it only establishes the baseline, which is what adding a
// watch to an existing directory needs: its current contents are not news.
func (b *kqueueBackend) rescanLocked(w *kqWatch, report bool) []Event {
	entries, err := os.ReadDir(w.path)
	if err != nil {
		// The directory itself has gone; its own descriptor reports that.
		if !errors.Is(err, os.ErrNotExist) && report {
			b.sink.fail(fmt.Errorf("notify: listing %s: %w", w.path, err))
		}
		return nil
	}

	current := make(map[string]fileID, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue // vanished between listing and inspection
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
		b.syncChildWatchesLocked(w, current)
	}
	return events
}

// syncChildWatchesLocked opens descriptors for new entries and closes them for
// entries that have gone. b.mu must be held.
func (b *kqueueBackend) syncChildWatchesLocked(w *kqWatch, current map[string]fileID) {
	for name := range current {
		path := filepath.Join(w.path, name)
		if b.byPath[path] != nil {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		// Watching a subdirectory's contents is what a recursive watch is for;
		// a plain directory watch reports only that the subdirectory exists.
		if info.IsDir() {
			continue
		}
		if _, err := b.watchLocked(path, false, false, w.opts); err != nil {
			// Running out of budget is not a failure to watch, it is a
			// decision to watch differently: the file is compared on an
			// interval instead, so modifications are reported late rather than
			// not at all.
			if errors.Is(err, errFDBudgetExhausted) {
				b.degraded.add(path, w.opts.ops)
				continue
			}
			// A dangling symlink cannot be opened, and a file may be
			// unreadable. Neither is a reason to stop watching the rest of the
			// directory, which is what failing here would amount to.
			debugf("not watching %s: %s", path, err)
			continue
		}
		// A file that now has a descriptor no longer needs comparing.
		b.degraded.remove(path)
	}

	for _, child := range b.childrenLocked(w.path) {
		if _, ok := current[filepath.Base(child.path)]; !ok {
			b.dropLocked(child)
		}
	}
}

// kqOpenError explains the failures an operator can act on.
//
// Descriptor exhaustion is the characteristic failure of this backend, and a
// bare EMFILE gives no hint that watching is what consumed them.
func kqOpenError(path string, err error) error {
	switch {
	case errors.Is(err, unix.EMFILE):
		return fmt.Errorf("%w: this process is out of file descriptors, and every watched "+
			"path needs one; raise the open file limit or watch fewer paths", err)
	case errors.Is(err, unix.ENFILE):
		return fmt.Errorf("%w: the system is out of file descriptors; raise kern.maxfiles", err)
	default:
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
}

func kqFlagsString(fflags uint32) string {
	bits := []struct {
		bit  uint32
		name string
	}{
		{unix.NOTE_DELETE, "DELETE"},
		{unix.NOTE_WRITE, "WRITE"},
		{unix.NOTE_EXTEND, "EXTEND"},
		{unix.NOTE_ATTRIB, "ATTRIB"},
		{unix.NOTE_LINK, "LINK"},
		{unix.NOTE_RENAME, "RENAME"},
		{unix.NOTE_REVOKE, "REVOKE"},
	}

	var out []string
	for _, x := range bits {
		if fflags&x.bit != 0 {
			out = append(out, x.name)
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("0x%x", fflags)
	}
	return joinStrings(out, "|")
}

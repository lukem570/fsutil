package notify

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The polling backend detects changes by rescanning watched paths on an
// interval and comparing the result with the previous scan.
//
// It is the universal fallback. Every other backend depends on the kernel
// telling us something; polling depends only on being able to stat, which is
// the one thing every filesystem supports. That makes it the only option for
// network and virtual filesystems, and the safety net when a native backend
// runs out of kernel resources.
//
// Its limitations follow directly from how it works, and cannot be engineered
// away:
//
//   - Latency is up to one interval.
//   - Changes that cancel out within one interval are invisible. A file
//     created and deleted between two scans is never reported.
//   - A modification that changes neither size nor modification time is
//     invisible. This is rare with nanosecond timestamps but real on
//     filesystems with one-second granularity.
//   - Cost is one lstat per watched path per interval.

func init() {
	caps := CapRecursive | CapNoFollow
	if fileIDSupported {
		// Pairing a removal with a creation requires stable file identity.
		caps |= CapPreciseRename
	}
	factoryCaps[BackendPoll] = caps

	register(backendFactory{
		kind: BackendPoll,
		// Lowest priority: correct everywhere, best nowhere. Any backend that
		// hears from the kernel beats it.
		priority: 0,
		new:      newPollBackend,
	})
}

// pollEntry is the recorded state of one path. It holds the least information
// that still distinguishes the changes we report.
type pollEntry struct {
	size  int64
	mtime time.Time
	mode  fs.FileMode
	isDir bool
	id    fileID
}

// changed reports how e differs from prev, as a set of operations.
func (e pollEntry) changed(prev pollEntry) Op {
	var op Op

	// A path whose identity changed is a different file wearing the same name,
	// which is what an atomic save looks like: write a temporary, rename it
	// over the target. Report it as a creation, because that is what the
	// kernel-backed backends report for the rename that produced it.
	if fileIDSupported && e.id != prev.id && e.id != (fileID{}) {
		op |= Create
	}

	if e.mode != prev.mode {
		op |= Chmod
	}

	// A directory's size and timestamp change as a side effect of entries
	// being added and removed inside it. Those changes are already reported as
	// Create and Remove on the entries themselves, so reporting them again as
	// a write to the directory would be noise that no kernel backend produces.
	if !e.isDir && (e.size != prev.size || !e.mtime.Equal(prev.mtime)) {
		op |= Write
	}

	return op
}

type pollWatch struct {
	opts addOpts
	snap map[string]pollEntry
}

type pollBackend struct {
	sink     sink
	interval time.Duration

	mu      sync.Mutex
	watches map[string]*pollWatch

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func newPollBackend(s sink, cfg config) (backend, error) {
	b := &pollBackend{
		sink:     s,
		interval: cfg.pollInterval,
		watches:  make(map[string]*pollWatch),
		done:     make(chan struct{}),
	}
	b.wg.Add(1)
	go b.run()
	return b, nil
}

func (b *pollBackend) Kind() Backend { return BackendPoll }

func (b *pollBackend) Capabilities() Capability { return factoryCaps[BackendPoll] }

// Add records a baseline scan of path and begins watching it.
//
// The baseline is taken synchronously so that files already present when the
// watch is added are not reported as creations on the first tick.
func (b *pollBackend) Add(path string, opts addOpts) error {
	snap, err := scan(path, opts)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.watches[path] = &pollWatch{opts: opts, snap: snap}
	return nil
}

func (b *pollBackend) Remove(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.watches[path]; !ok {
		return ErrNonExistentWatch
	}
	delete(b.watches, path)
	return nil
}

func (b *pollBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.watches))
	for p := range b.watches {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (b *pollBackend) Close() error {
	b.closeOnce.Do(func() { close(b.done) })
	b.wg.Wait()
	return nil
}

func (b *pollBackend) run() {
	defer b.wg.Done()

	t := time.NewTicker(b.interval)
	defer t.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-t.C:
			if !b.tick() {
				return
			}
		}
	}
}

// tick rescans every watch once. It reports false when the watcher is shutting
// down and the loop should stop.
//
// Scanning and sending both happen with the lock released: a scan of a large
// tree is slow, and a send blocks until the consumer is ready. Holding the
// lock across either would stall Add and Remove for arbitrarily long.
func (b *pollBackend) tick() bool {
	// Events are gathered across every watch before any are sent, so that a
	// path covered by two overlapping watches is reported once rather than
	// once per watch. Backends that receive events from the kernel get this
	// for free — the kernel reports a change to a file, not to each watch that
	// happens to include it — and a caller should not be able to tell which
	// backend it is using by counting events.
	var (
		events []Event
		byPath = make(map[string]int)
	)
	record := func(ev Event) {
		if i, seen := byPath[ev.Name]; seen {
			events[i].Op |= ev.Op
			return
		}
		byPath[ev.Name] = len(events)
		events = append(events, ev)
	}

	for _, path := range b.WatchList() {
		b.mu.Lock()
		wch, ok := b.watches[path]
		var (
			prev map[string]pollEntry
			opts addOpts
		)
		if ok {
			prev, opts = wch.snap, wch.opts
		}
		b.mu.Unlock()
		if !ok {
			continue // removed while we were working
		}

		next, err := scan(path, opts)
		if err != nil {
			// The watched path itself disappearing is a normal event, not a
			// failure to watch.
			if errors.Is(err, fs.ErrNotExist) {
				b.mu.Lock()
				delete(b.watches, path)
				b.mu.Unlock()
				if opts.ops.Has(Remove) {
					record(Event{Name: path, Op: Remove})
				}
				continue
			}
			if !b.sink.fail(err) {
				return false
			}
			continue
		}

		for _, ev := range diffScans(prev, next, opts.ops) {
			record(ev)
		}

		b.mu.Lock()
		// Re-check: the watch may have been removed or replaced during the
		// scan, in which case this snapshot is stale and must be discarded.
		if cur, still := b.watches[path]; still && cur == wch {
			cur.snap = next
		}
		b.mu.Unlock()
	}

	for _, ev := range events {
		if !b.sink.send(ev) {
			return false
		}
	}
	return true
}

// diffScans converts the difference between two scans into events, filtered to
// the requested operations.
//
// Events are ordered by path so that a given filesystem change always produces
// the same sequence. Nothing about polling makes that ordering meaningful, but
// determinism makes the behaviour testable.
func diffScans(prev, next map[string]pollEntry, ops Op) []Event {
	var created, removed []string
	for path := range next {
		if _, ok := prev[path]; !ok {
			created = append(created, path)
		}
	}
	for path := range prev {
		if _, ok := next[path]; !ok {
			removed = append(removed, path)
		}
	}
	sort.Strings(created)
	sort.Strings(removed)

	events := make([]Event, 0, len(created)+len(removed))
	renamed := pairRenames(prev, next, created, removed)

	for _, path := range removed {
		// A path that vanished from one place and reappeared in another with
		// the same identity was moved, not deleted.
		if _, ok := renamed[path]; ok {
			events = append(events, Event{Name: path, Op: Rename})
			continue
		}
		events = append(events, Event{Name: path, Op: Remove})
	}

	for _, path := range created {
		events = append(events, Event{Name: path, Op: Create})
	}

	changed := make([]string, 0, len(next))
	for path, entry := range next {
		if before, ok := prev[path]; ok {
			if op := entry.changed(before); op != 0 {
				changed = append(changed, path)
			}
		}
	}
	sort.Strings(changed)
	for _, path := range changed {
		events = append(events, Event{Name: path, Op: next[path].changed(prev[path])})
	}

	return filterOps(events, ops)
}

// pairRenames matches removed paths to created paths by file identity, and
// returns the set of removed paths that were in fact moved.
//
// Identity is the only sound basis for this: matching on size or name would
// pair unrelated files that happen to coincide. Where identity is unavailable
// the result is empty, and a move is reported as a removal plus a creation —
// less precise, but never wrong.
func pairRenames(prev, next map[string]pollEntry, created, removed []string) map[string]string {
	if !fileIDSupported {
		return nil
	}

	byID := make(map[fileID]string, len(created))
	for _, path := range created {
		if id := next[path].id; id != (fileID{}) {
			byID[id] = path
		}
	}
	if len(byID) == 0 {
		return nil
	}

	pairs := make(map[string]string)
	for _, path := range removed {
		id := prev[path].id
		if id == (fileID{}) {
			continue
		}
		if dst, ok := byID[id]; ok {
			pairs[path] = dst
		}
	}
	return pairs
}

// filterOps drops events outside the requested operation set, and drops any
// event left with nothing to report.
func filterOps(events []Event, ops Op) []Event {
	out := events[:0]
	for _, ev := range events {
		if masked := ev.Op & ops; masked != 0 {
			ev.Op = masked
			out = append(out, ev)
		}
	}
	return out
}

// scan records the current state of a watched path.
//
// For a file this is the file itself. For a directory it is the directory and
// its entries, descending into subdirectories only when the watch is
// recursive.
func scan(root string, opts addOpts) (map[string]pollEntry, error) {
	info, err := statPath(root, opts.noFollow)
	if err != nil {
		return nil, err
	}

	out := map[string]pollEntry{root: entryOf(info)}
	if !info.IsDir() {
		return out, nil
	}
	if opts.recursive {
		return out, scanTree(root, out, opts)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// Removed between listing the directory and inspecting the entry.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		full := filepath.Join(root, entry.Name())
		if opts.excluded(root, full) {
			continue
		}
		out[full] = entryOf(info)
	}
	return out, nil
}

// scanTree walks a directory tree into out.
//
// The walk is confined to root by [os.Root], which resolves every component
// relative to an open directory handle. A plain [filepath.Walk] would decline
// to follow symlinks but would still be vulnerable to a directory being
// swapped for a symlink mid-walk; confinement removes that race rather than
// narrowing it.
func scanTree(root string, out map[string]pollEntry, opts addOpts) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()

	return fs.WalkDir(dir.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Trees change while being walked. A path that has gone is not an
			// error; it is the change we exist to report, and the next scan
			// will see it too.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if path == "." {
			return nil // the root is already recorded
		}

		full := filepath.Join(root, filepath.FromSlash(path))
		if opts.excluded(root, full) {
			// Prune, so an excluded directory costs nothing to skip rather
			// than being stat-ed on every tick.
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out[full] = entryOf(info)
		return nil
	})
}

// statPath stats a path, following symlinks unless noFollow is set.
func statPath(path string, noFollow bool) (fs.FileInfo, error) {
	if noFollow {
		return os.Lstat(path)
	}
	return os.Stat(path)
}

func entryOf(info fs.FileInfo) pollEntry {
	id, _ := statID(info)
	return pollEntry{
		size:  info.Size(),
		mtime: info.ModTime(),
		mode:  info.Mode(),
		isDir: info.IsDir(),
		id:    id,
	}
}

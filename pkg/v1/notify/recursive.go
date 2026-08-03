package notify

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The recursive wrapper extends a single-directory backend to a whole tree.
//
// Most kernel interfaces watch one directory at a time. Turning that into a
// recursive watch means placing a watch on every directory in the tree,
// adopting directories as they are created, and pruning them as they are
// removed. None of that work depends on which interface delivered the event,
// so it is done once here rather than once per platform.
//
// The hard part is a race that no amount of care inside a backend can avoid.
// A directory is created, and the kernel tells us about it — but by the time
// we have been told, files may already exist inside it, and we were not
// watching when they appeared. Watching the new directory from that moment on
// is not enough; its contents have to be examined as well. That is why
// adopting a directory both places a watch and walks what is already there.
//
// Doing so introduces the opposite problem. A file created just as the watch
// goes on may be found by the walk *and* reported by the kernel, which would
// deliver it twice. Paths reported by a walk are therefore recorded briefly,
// and the first matching kernel event for each is dropped.

// synthesizedTTL is how long a path found by a walk suppresses a duplicate
// kernel event for the same path.
//
// It only has to cover the gap between placing a watch and finishing the walk,
// which is microseconds. Seconds of margin costs nothing and removes any doubt
// on a loaded machine.
const synthesizedTTL = 5 * time.Second

// recRoot is one recursive watch: the tree the caller asked for, and the
// directories currently watched to cover it.
type recRoot struct {
	opts addOpts
	dirs map[string]bool
}

type recursiveBackend struct {
	inner backend
	out   sink

	mu sync.Mutex
	// roots holds recursive watches, keyed by the path the caller added.
	roots map[string]*recRoot
	// plain holds non-recursive watches, which pass straight through.
	plain map[string]bool
	// synthesized records paths already reported by a walk, so that a kernel
	// event for the same path can be recognised as a duplicate and dropped.
	synthesized map[string]time.Time
	// dirRefs counts how many recursive watches need each inner watch.
	//
	// Two recursive watches can cover the same directory — one nested inside
	// the other, or two siblings whose trees overlap through a link. Without a
	// count, removing either would tear down the inner watch the other still
	// depends on, and that watch would go quiet with nothing reporting an
	// error.
	dirRefs map[string]int
}

func newRecursiveBackend(f backendFactory, out sink, cfg config) (backend, error) {
	b := &recursiveBackend{
		out:         out,
		roots:       make(map[string]*recRoot),
		plain:       make(map[string]bool),
		synthesized: make(map[string]time.Time),
		dirRefs:     make(map[string]int),
	}

	inner, err := f.new(recursiveSink{b}, cfg)
	if err != nil {
		return nil, err
	}
	b.inner = inner
	return b, nil
}

// retain establishes an inner watch on dir for one recursive watch, sharing an
// existing one if another already needs it.
func (b *recursiveBackend) retain(dir string, opts addOpts) error {
	b.mu.Lock()
	b.dirRefs[dir]++
	first := b.dirRefs[dir] == 1
	b.mu.Unlock()

	if !first {
		return nil
	}
	if err := b.inner.Add(dir, opts); err != nil {
		b.mu.Lock()
		if b.dirRefs[dir]--; b.dirRefs[dir] <= 0 {
			delete(b.dirRefs, dir)
		}
		b.mu.Unlock()
		return err
	}
	return nil
}

// release gives up one recursive watch's interest in an inner watch, removing
// it only once nothing needs it.
func (b *recursiveBackend) release(dir string) {
	b.mu.Lock()
	b.dirRefs[dir]--
	last := b.dirRefs[dir] <= 0
	if last {
		delete(b.dirRefs, dir)
	}
	b.mu.Unlock()

	if !last {
		return
	}
	// The kernel has usually dropped this already, when the directory it
	// referred to no longer exists.
	if err := b.inner.Remove(dir); err != nil && !errors.Is(err, ErrNonExistentWatch) {
		debugf("releasing watch on %s: %s", dir, err)
	}
}

// recursiveSink is the sink the wrapped backend delivers to.
//
// It is a distinct type rather than methods on recursiveBackend so that the
// backend's own exported surface cannot be mistaken for a sink, and so that
// the direction of delivery is obvious at the call site.
type recursiveSink struct{ b *recursiveBackend }

func (s recursiveSink) send(ev Event) bool  { return s.b.onEvent(ev) }
func (s recursiveSink) fail(err error) bool { return s.b.out.fail(err) }
func (s recursiveSink) closing() bool       { return s.b.out.closing() }

func (b *recursiveBackend) Kind() Backend { return b.inner.Kind() }

// Capabilities reports the wrapped backend's own capabilities plus recursion,
// which is what this type adds.
func (b *recursiveBackend) Capabilities() Capability {
	return b.inner.Capabilities() | CapRecursive
}

func (b *recursiveBackend) Close() error { return b.inner.Close() }

// budgetStats forwards descriptor accounting from the wrapped backend.
//
// Without this the wrapper would hide it: the watcher asks whichever backend
// it holds, and on every platform with a per-path descriptor cost that backend
// is this wrapper rather than the one actually spending them.
func (b *recursiveBackend) budgetStats() (held, budget, denied, limit int) {
	if bb, ok := b.inner.(budgeter); ok {
		return bb.budgetStats()
	}
	return 0, 0, 0, 0
}

func (b *recursiveBackend) Add(path string, opts addOpts) error {
	if !opts.recursive {
		if err := b.inner.Add(path, opts); err != nil {
			return err
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.plain[path] = true
		return nil
	}

	dirs, err := collectDirs(path)
	if err != nil {
		return err
	}

	added := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if err := b.retain(dir, opts); err != nil {
			// Leave nothing half-established: a partially watched tree would
			// report some changes and silently miss others, which is worse
			// than reporting none.
			for d := range added {
				b.release(d)
			}
			return err
		}
		added[dir] = true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.roots[path] = &recRoot{opts: opts, dirs: added}
	return nil
}

func (b *recursiveBackend) Remove(path string) error {
	b.mu.Lock()
	root, isRoot := b.roots[path]
	if isRoot {
		delete(b.roots, path)
	}
	isPlain := b.plain[path]
	if isPlain {
		delete(b.plain, path)
	}
	b.mu.Unlock()

	switch {
	case isRoot:
		dirs := make([]string, 0, len(root.dirs))
		for d := range root.dirs {
			dirs = append(dirs, d)
		}
		// Deepest first, so a backend that cares about ordering never sees a
		// parent removed while its children are still registered.
		sort.Sort(sort.Reverse(sort.StringSlice(dirs)))

		for _, d := range dirs {
			// A directory that has already gone was watched until it did; the
			// kernel dropped that watch for us, and another recursive watch may
			// still need this one, so release rather than remove.
			b.release(d)
		}
		return nil

	case isPlain:
		return b.inner.Remove(path)

	default:
		return ErrNonExistentWatch
	}
}

// WatchList reports the paths the caller added, not the directories watched
// internally to cover them. A recursive watch on a tree of a thousand
// directories is one entry, because that is what the caller asked for and what
// they would pass to Remove.
func (b *recursiveBackend) WatchList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.roots)+len(b.plain))
	for p := range b.roots {
		out = append(out, p)
	}
	for p := range b.plain {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// onEvent handles one event from the wrapped backend.
//
// It runs on the backend's own delivery goroutine, which is why backends must
// not hold locks while delivering: the work here calls back into the backend
// to add and remove watches.
func (b *recursiveBackend) onEvent(ev Event) bool {
	root, rootPath := b.rootFor(ev.Name)

	if ev.Has(Remove) || ev.Has(Rename) {
		// Prune before forgetting: pruning needs the root's directory set,
		// and forgetting discards it.
		if root != nil {
			b.prune(rootPath, ev.Name)
		}
		b.forget(ev.Name)
	}

	if root == nil {
		return b.out.send(ev) // not part of a recursive watch
	}

	if ev.Has(Create) && b.consumeSynthesized(ev.Name) {
		// Already reported when the containing directory was adopted.
		debugf("dropping duplicate create for %s", ev.Name)
		return true
	}

	if ev.Has(Create) && isDir(ev.Name) {
		// Place the watch before announcing the directory, so that anything
		// created in it from this moment on is reported. Announcing first
		// would leave a window in which the caller knows the directory exists
		// but changes inside it are still invisible.
		found := b.adopt(rootPath, ev.Name)

		if !b.out.send(ev) {
			return false
		}
		// Anything already inside the new directory appeared before we could
		// watch it, so the kernel will never report it. Report it here — but
		// only if this watch asked for creations at all, since a synthesised
		// event must respect the same filter as a real one.
		if root.opts.ops.Has(Create) {
			for _, path := range found {
				if !b.out.send(Event{Name: path, Op: Create}) {
					return false
				}
			}
		}
		return true
	}

	return b.out.send(ev)
}

// rootFor finds the recursive watch covering path, if any.
//
// The longest match wins, so nesting one recursive watch inside another
// attributes an event to the more specific of the two.
func (b *recursiveBackend) rootFor(path string) (*recRoot, string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var (
		best     *recRoot
		bestPath string
	)
	for rootPath, root := range b.roots {
		if !underOrEqual(rootPath, path) {
			continue
		}
		if best == nil || len(rootPath) > len(bestPath) {
			best, bestPath = root, rootPath
		}
	}
	return best, bestPath
}

// adopt places watches on a newly appeared directory and everything beneath
// it, and returns the paths found inside — the ones that existed before we
// could watch for them.
func (b *recursiveBackend) adopt(rootPath, dir string) []string {
	b.mu.Lock()
	root := b.roots[rootPath]
	if root == nil {
		b.mu.Unlock()
		return nil
	}
	opts := root.opts
	b.mu.Unlock()

	dirs, err := collectDirs(dir)
	if err != nil {
		// The directory may have been removed again already, which is not
		// worth reporting as a failure of the watch.
		if !errors.Is(err, fs.ErrNotExist) {
			b.out.fail(err)
		}
		return nil
	}

	for _, d := range dirs {
		if err := b.retain(d, opts); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				b.out.fail(err)
			}
			continue
		}
		b.mu.Lock()
		if r := b.roots[rootPath]; r != nil {
			r.dirs[d] = true
		}
		b.mu.Unlock()
	}

	found := collectEntries(dir)
	if len(found) > 0 {
		b.markSynthesized(found)
	}
	return found
}

// prune drops the watches covering a path that has been removed or moved away,
// along with everything that was beneath it.
func (b *recursiveBackend) prune(rootPath, path string) {
	b.mu.Lock()
	root := b.roots[rootPath]
	if root == nil {
		b.mu.Unlock()
		return
	}

	var gone []string
	for dir := range root.dirs {
		if underOrEqual(path, dir) {
			gone = append(gone, dir)
			delete(root.dirs, dir)
		}
	}
	b.mu.Unlock()

	for _, dir := range gone {
		b.release(dir)
	}
}

// forget drops a watch whose path has itself been removed or moved away.
//
// The kernel has already discarded the underlying watch — the thing it was
// watching is gone — so keeping the entry would make WatchList a record of
// what was once asked for rather than of what is actually being watched.
//
// Only an event naming the watched path itself matters here. A file removed
// from inside a watched directory does not end the directory's watch.
func (b *recursiveBackend) forget(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.plain, path)
	delete(b.roots, path)
}

// markSynthesized records paths reported by a walk so that a kernel event for
// the same path can be recognised as a duplicate.
func (b *recursiveBackend) markSynthesized(paths []string) {
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Expire stale entries whenever the map is touched. Nothing else sweeps
	// it, and a watcher on a busy tree would otherwise accumulate an entry per
	// file adopted for the life of the process.
	for path, at := range b.synthesized {
		if now.Sub(at) > synthesizedTTL {
			delete(b.synthesized, path)
		}
	}
	for _, path := range paths {
		b.synthesized[path] = now
	}
}

// consumeSynthesized reports whether path was already announced by a walk, and
// clears the record if so.
//
// The record is consumed rather than merely read: it suppresses exactly one
// event. A file that is created, deleted, and created again should be reported
// the second time.
func (b *recursiveBackend) consumeSynthesized(path string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	at, ok := b.synthesized[path]
	if !ok {
		return false
	}
	delete(b.synthesized, path)
	return time.Since(at) <= synthesizedTTL
}

// collectDirs returns path and every directory beneath it.
//
// The walk is confined by [os.Root], so a symlink inside the tree cannot lead
// it outside. A recursive watch that followed symlinks would place watches on
// arbitrary parts of the filesystem, which is both surprising and an easy way
// to exhaust the kernel's watch limit.
func collectDirs(path string) ([]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}

	dir, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	dirs := []string{path}
	err = fs.WalkDir(dir.FS(), ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Trees change while being walked, and a directory we cannot read
			// is not a reason to refuse to watch the rest of the tree.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if p == "." || !entry.IsDir() {
			return nil
		}
		dirs = append(dirs, filepath.Join(path, filepath.FromSlash(p)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dirs, nil
}

// collectEntries returns every path beneath dir, excluding dir itself.
func collectEntries(dir string) []string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()

	var out []string
	_ = fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if p == "." {
			return nil
		}
		out = append(out, filepath.Join(dir, filepath.FromSlash(p)))
		return nil
	})
	sort.Strings(out)
	return out
}

// underOrEqual reports whether path is parent or lies beneath it.
//
// It compares path components rather than strings, so that "/a/bc" is not
// mistaken for something inside "/a/b".
func underOrEqual(parent, path string) bool {
	if parent == path {
		return true
	}
	if !strings.HasSuffix(parent, string(filepath.Separator)) {
		parent += string(filepath.Separator)
	}
	return strings.HasPrefix(path, parent)
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

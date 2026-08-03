package notify

import (
	"fmt"
	path2 "path"
	"path/filepath"
	"strings"
	"time"
)

// recursiveSuffix marks a path as recursive when appended to it, as in
// "/var/log/...". It mirrors the notation Go itself uses for package patterns.
const recursiveSuffix = "..."

// defaultPollInterval is how often the polling backend rescans. It is a
// compromise: short enough that a human notices the latency only rarely, long
// enough that watching a large tree does not saturate a core with stat calls.
const defaultPollInterval = 500 * time.Millisecond

// config holds watcher-wide settings assembled from [Option] values.
type config struct {
	backend      Backend
	eventBuffer  uint
	pollInterval time.Duration
	fdBudget     int // 0 means "derive from the process limit"
}

func defaultConfig() config {
	return config{
		backend:      BackendAuto,
		pollInterval: defaultPollInterval,
	}
}

// Option configures a [Watcher] at construction time. Options are passed to
// [NewWatcherWith].
type Option interface{ apply(*config) }

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

// WithBackend requests a specific notification backend.
//
// The default, [BackendAuto], picks the best mechanism the host provides and
// falls back to polling. Naming a backend explicitly disables that fallback:
// if the host cannot provide it, [NewWatcherWith] fails with [ErrUnsupported]
// rather than quietly giving you something slower or less precise.
//
// Use [Backends] to discover what is available at runtime.
func WithBackend(b Backend) Option {
	return optionFunc(func(c *config) { c.backend = b })
}

// WithPollInterval sets how often the polling backend rescans watched paths.
//
// It has no effect on backends that receive events from the kernel. Shorter
// intervals reduce latency and increase CPU cost linearly with the number of
// watched paths; the cost is one lstat per path per interval.
func WithPollInterval(d time.Duration) Option {
	return optionFunc(func(c *config) { c.pollInterval = d })
}

// WithEventBuffer sets the capacity of the Events channel, as
// [NewBufferedWatcher] does.
//
// A buffer decouples the watcher from a slow consumer, but it cannot make an
// unbounded stream of events fit in bounded memory: if the consumer is
// persistently slower than the filesystem, a buffer only delays the point at
// which events are dropped. Prefer a consumer that hands work off promptly.
func WithEventBuffer(size uint) Option {
	return optionFunc(func(c *config) { c.eventBuffer = size })
}

// WithFDBudget caps how many file descriptors the watcher may hold.
//
// Backends that consume a descriptor per watched path stay within this budget
// and serve any excess by polling, so the number of watched paths is not
// bounded by the process descriptor limit. The default derives a budget from
// that limit while reserving headroom for the rest of the program.
//
// A budget of zero restores the default. Negative values are rejected.
func WithFDBudget(n int) Option {
	return optionFunc(func(c *config) { c.fdBudget = n })
}

// validate reports whether the assembled configuration is usable.
func (c *config) validate() error {
	if c.pollInterval <= 0 {
		return fmt.Errorf("notify: poll interval must be positive, got %s", c.pollInterval)
	}
	if c.fdBudget < 0 {
		return fmt.Errorf("notify: fd budget must not be negative, got %d", c.fdBudget)
	}
	return nil
}

// addOpts holds per-watch settings assembled from [AddOption] values.
type addOpts struct {
	ops        Op
	bufferSize int
	noFollow   bool
	recursive  bool
	exclude    []string
}

func defaultAddOpts() addOpts {
	return addOpts{
		ops:        portableOps,
		bufferSize: defaultBufferSize,
	}
}

// defaultBufferSize is the per-watch kernel buffer used by backends that read
// events into one. 64 KiB holds a few hundred events of typical path length.
const defaultBufferSize = 64 * 1024

// AddOption configures a single watch. Options are passed to [Watcher.AddWith].
//
// The type is exported but its method is not, which is deliberate. Callers
// need to name the type — assembling a list of options from configuration and
// passing it on is ordinary — but nothing outside this package can implement
// it, so the option set can grow without breaking anyone.
type AddOption interface{ applyAdd(*addOpts) }

type addOptFunc func(*addOpts)

func (f addOptFunc) applyAdd(o *addOpts) { f(o) }

// WithBufferSize sets the kernel buffer size, in bytes, for a single watch.
//
// It applies only to backends that read events into a userspace buffer, which
// today means the Windows directory-change backend. It is accepted and ignored
// elsewhere, so that portable code can set it unconditionally.
//
// Raise it when watching a directory that changes in large bursts: if the
// buffer fills before the watcher drains it, the kernel discards the overflow
// and reports [ErrEventOverflow] on the Errors channel.
func WithBufferSize(bytes int) AddOption {
	return addOptFunc(func(o *addOpts) { o.bufferSize = bytes })
}

// WithOps restricts a watch to the given operations.
//
// Filtering happens as close to the kernel as the backend allows, so narrowing
// the set can reduce work rather than merely hiding events. Passing no
// operations is an error at Add time.
//
// This is also how the Unportable operations are requested; they are never
// delivered by default. Requesting one on a backend that cannot produce it
// fails with [ErrUnsupported] rather than silently never firing:
//
//	err := w.AddWith("/srv/upload", notify.WithOps(notify.UnportableCloseWrite))
func WithOps(ops ...Op) AddOption {
	return addOptFunc(func(o *addOpts) {
		var mask Op
		for _, op := range ops {
			mask |= op
		}
		o.ops = mask
	})
}

// WithNoFollow watches a symlink itself rather than its target.
//
// By default, adding a symlink watches whatever it points at. With this option
// the link is watched directly, so the watch reports the link being replaced
// or removed and reports nothing about the target.
func WithNoFollow() AddOption {
	return addOptFunc(func(o *addOpts) { o.noFollow = true })
}

// WithRecursive watches a directory and all of its descendants, including
// directories created after the watch is established.
//
// It is equivalent to appending "/..." to the path. Recursion is not free:
// backends without native support maintain a watch per directory, so a deep
// tree consumes proportionally many kernel resources. See [WithFDBudget].
func WithRecursive() AddOption {
	return addOptFunc(func(o *addOpts) { o.recursive = true })
}

// WithExclude skips paths matching any of the given patterns.
//
// Patterns use [path.Match] syntax and are tested against two things: each
// path's final element, and its path relative to the watch root, written with
// forward slashes on every platform. So "node_modules" excludes that directory
// wherever it appears, while "build/cache" excludes only that one place.
//
// Excluding a directory prunes it entirely. Nothing beneath it is watched, and
// no watch is placed on it, which is the point: watching a repository without
// skipping its version-control directory spends kernel watches on thousands of
// files nobody wants events for, and buries the changes that matter under
// churn from the tool tracking them.
//
//	w.AddWith(repo, notify.WithRecursive(),
//		notify.WithExclude(".git", "node_modules", "*.tmp"))
//
// Exclusions apply to recursive watches, where they decide what is walked, and
// to events, so a backend that reports a path anyway does not deliver it.
// Adding an excluded path directly is still honoured — the option describes
// what to skip while descending, not what may be watched.
func WithExclude(patterns ...string) AddOption {
	return addOptFunc(func(o *addOpts) { o.exclude = append(o.exclude, patterns...) })
}

// excluded reports whether path, which lies under root, matches any exclusion.
func (o *addOpts) excluded(root, path string) bool {
	if len(o.exclude) == 0 {
		return false
	}

	base := filepath.Base(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = ""
	}
	rel = filepath.ToSlash(rel)

	for _, pattern := range o.exclude {
		if ok, err := path2.Match(pattern, base); err == nil && ok {
			return true
		}
		if rel != "" && rel != "." {
			if ok, err := path2.Match(pattern, rel); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// validate reports whether the assembled watch options are usable, given what
// the backend can actually do.
func (o *addOpts) validate(caps Capability) error {
	if o.ops == 0 {
		return fmt.Errorf("notify: %w: WithOps was given no operations", ErrUnsupported)
	}
	if unknown := o.ops &^ allOps; unknown != 0 {
		return fmt.Errorf("notify: unknown operation in WithOps: 0x%x", uint32(unknown))
	}
	if o.bufferSize <= 0 {
		return fmt.Errorf("notify: buffer size must be positive, got %d", o.bufferSize)
	}
	if want := o.ops & unportableOps; want != 0 && !caps.Has(CapUnportableOps) {
		return fmt.Errorf("notify: %w: this backend cannot report %s", ErrUnsupported, want)
	}
	for _, pattern := range o.exclude {
		// Report a malformed pattern when it is given rather than letting it
		// silently match nothing, which would look like the exclusion working
		// until someone noticed the events it should have suppressed.
		if _, err := path2.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("notify: invalid exclude pattern %q: %w", pattern, err)
		}
	}
	if o.noFollow && !caps.Has(CapNoFollow) {
		return fmt.Errorf("notify: %w: this backend cannot watch symlinks without following them", ErrUnsupported)
	}
	return nil
}

// splitRecursive strips a trailing "/..." from path and reports whether it was
// present. The returned path is cleaned.
//
// A bare "..." means the current directory, recursively. Note that this is
// unambiguous: a real directory entry can never be named "..." because the
// component is stripped by [filepath.Clean] only in this position, and no
// filesystem allows a component consisting solely of dots beyond "." and "..".
func splitRecursive(path string) (string, bool) {
	if path == recursiveSuffix {
		return ".", true
	}

	sep := string(filepath.Separator)
	// Accept a forward slash on every platform: callers write "/tmp/x/..."
	// in portable code, and Windows accepts forward slashes throughout.
	if strings.HasSuffix(path, sep+recursiveSuffix) || strings.HasSuffix(path, "/"+recursiveSuffix) {
		trimmed := path[:len(path)-len(recursiveSuffix)-1]
		if trimmed == "" {
			trimmed = sep
		}
		return filepath.Clean(trimmed), true
	}
	return filepath.Clean(path), false
}

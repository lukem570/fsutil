package notify

import (
	"fmt"
	"sort"
)

// Backend identifies a filesystem notification mechanism.
type Backend uint8

const (
	// BackendAuto selects the best available backend for the host. It is the
	// default and is never reported by [Watcher.Backend].
	BackendAuto Backend = iota

	// BackendINotify is the Linux inotify interface.
	BackendINotify

	// BackendFANotify is the Linux fanotify interface. It can watch an entire
	// mount or filesystem with a single mark, but requires privileges.
	BackendFANotify

	// BackendKqueue is the BSD and macOS kqueue interface. It reports changes
	// per open file descriptor, so watching many files is expensive.
	BackendKqueue

	// BackendDirectoryChanges is the Windows directory-change interface.
	BackendDirectoryChanges

	// BackendUSNJournal is the Windows NTFS change journal. It observes a whole
	// volume without per-directory handles, but requires elevation.
	BackendUSNJournal

	// BackendFEN is the illumos and Solaris file event notification interface.
	BackendFEN

	// BackendPoll periodically rescans watched paths. It works everywhere,
	// including on network and virtual filesystems that deliver no events at
	// all, at the cost of latency and CPU proportional to the tree size.
	BackendPoll
)

var backendNames = map[Backend]string{
	BackendAuto:             "auto",
	BackendINotify:          "inotify",
	BackendFANotify:         "fanotify",
	BackendKqueue:           "kqueue",
	BackendDirectoryChanges: "directory-changes",
	BackendUSNJournal:       "usn-journal",
	BackendFEN:              "fen",
	BackendPoll:             "poll",
}

// String returns the backend's name, such as "inotify".
func (b Backend) String() string {
	if n, ok := backendNames[b]; ok {
		return n
	}
	return fmt.Sprintf("Backend(%d)", uint8(b))
}

// Capability is a bitmask describing what a backend can do. Capabilities vary
// by operating system and, for some backends, by kernel version, so they are
// reported at runtime rather than assumed at compile time.
type Capability uint32

const (
	// CapRecursive means the backend watches a directory tree natively, so
	// recursion costs no extra kernel resources.
	CapRecursive Capability = 1 << iota

	// CapUnportableOps means the backend can report the Unportable operations,
	// such as [UnportableCloseWrite].
	CapUnportableOps

	// CapNoFollow means the backend can watch a symlink itself rather than its
	// target. See [WithNoFollow].
	CapNoFollow

	// CapPrivileged means the backend requires elevated privileges, so it is
	// never chosen automatically.
	CapPrivileged

	// CapFDPerPath means the backend consumes one file descriptor for every
	// watched path, making the watch count subject to the process descriptor
	// limit. Backends with this capability are subject to the fd budget.
	CapFDPerPath

	// CapFDPerDir means the backend consumes one descriptor per watched
	// directory regardless of how many files it contains, which is far cheaper
	// than [CapFDPerPath] for large trees.
	CapFDPerDir

	// CapPreciseRename means the backend can pair the two halves of a rename,
	// rather than inferring them from a removal and a creation observed close
	// together.
	CapPreciseRename
)

// Has reports whether c contains all of the capabilities in h.
func (c Capability) Has(h Capability) bool { return c&h == h }

// backend is the interface every platform implementation satisfies. It is
// unexported: backends are chosen by the package, not supplied by callers.
type backend interface {
	// Add begins watching path with the given options. The path has already
	// been cleaned and, for a recursive request, stripped of its "/..." suffix.
	Add(path string, opts addOpts) error

	// Remove stops watching path, returning ErrNonExistentWatch if it was not
	// being watched.
	Remove(path string) error

	// WatchList returns the paths currently being watched, in the form they
	// were added.
	WatchList() []string

	// Close releases every watch and all associated resources.
	//
	// It must not return until every goroutine capable of sending on the sink
	// has exited: the caller closes the event and error channels as soon as
	// Close returns, and a send on a closed channel panics.
	Close() error

	// Kind reports which backend this is.
	Kind() Backend

	// Capabilities reports what this backend can do on this host.
	Capabilities() Capability
}

// sink is how a backend delivers events and errors to the watcher's channels.
//
// Every send races with [Watcher.Close]; the done channel is the tiebreaker.
// Backends must use these methods rather than sending directly, and must treat
// a false return as "the watcher is shutting down, stop and return".
type sink struct {
	events chan<- Event
	errors chan<- error
	done   <-chan struct{}
}

// send delivers ev, reporting false if the watcher is closing.
func (s sink) send(ev Event) bool {
	if debugEnabled {
		debugf("event %s", ev)
	}
	select {
	case s.events <- ev:
		return true
	case <-s.done:
		return false
	}
}

// fail delivers err, reporting false if the watcher is closing.
func (s sink) fail(err error) bool {
	if debugEnabled {
		debugf("error %s", err)
	}
	select {
	case s.errors <- err:
		return true
	case <-s.done:
		return false
	}
}

// closing reports whether the watcher has begun shutting down. Backends use it
// to break out of loops that are not currently blocked on a send.
func (s sink) closing() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// backendFactory describes a backend implementation to the selection logic.
// Platform files register one from an init function.
type backendFactory struct {
	// kind identifies the backend.
	kind Backend

	// priority orders automatic selection; the highest available wins.
	priority int

	// available reports whether this host can actually use the backend right
	// now. Compilation is not enough: fanotify needs a recent kernel and
	// privileges, and the USN journal needs elevation.
	available func() bool

	// new constructs the backend.
	new func(s sink, cfg config) (backend, error)
}

// registry holds every backend compiled into this binary. It is populated from
// init functions and read-only thereafter, so it needs no lock.
var registry []backendFactory

// register adds a backend to the registry. It is called only from init.
func register(f backendFactory) {
	if f.available == nil {
		f.available = func() bool { return true }
	}
	registry = append(registry, f)
}

// Backends returns the backends usable on this host, best first.
//
// The result reflects the running system, not merely what was compiled in: a
// backend that needs privileges or a newer kernel is omitted when it is
// unavailable. It is intended for diagnostics and for deciding what to pass to
// [WithBackend].
func Backends() []Backend {
	fs := availableFactories()
	out := make([]Backend, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.kind)
	}
	return out
}

// availableFactories returns usable factories ordered by descending priority.
func availableFactories() []backendFactory {
	out := make([]backendFactory, 0, len(registry))
	for _, f := range registry {
		if f.available() {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].priority > out[j].priority })
	return out
}

// newBackend constructs the backend named by cfg, or the best available one.
func newBackend(s sink, cfg config) (backend, error) {
	avail := availableFactories()

	if cfg.backend == BackendAuto {
		for _, f := range avail {
			// A privileged backend is never chosen automatically: it may be
			// available only because the process happens to be running as
			// root, which is not a property to silently depend on.
			if caps := probeCapabilities(f); caps.Has(CapPrivileged) {
				continue
			}
			b, err := f.new(s, cfg)
			if err != nil {
				debugf("backend %s unavailable: %s", f.kind, err)
				continue
			}
			debugf("selected backend %s", f.kind)
			return b, nil
		}
		return nil, fmt.Errorf("notify: %w: no usable backend on this host", ErrUnsupported)
	}

	for _, f := range avail {
		if f.kind != cfg.backend {
			continue
		}
		b, err := f.new(s, cfg)
		if err != nil {
			return nil, fmt.Errorf("notify: backend %s: %w", cfg.backend, err)
		}
		return b, nil
	}

	// Distinguish "not built for this platform" from "built but unusable
	// here", because the fix differs: the first needs a different build, the
	// second needs privileges or a newer kernel.
	for _, f := range registry {
		if f.kind == cfg.backend {
			return nil, fmt.Errorf("notify: %w: backend %s is not usable on this host",
				ErrUnsupported, cfg.backend)
		}
	}
	return nil, fmt.Errorf("notify: %w: backend %s is not compiled into this binary",
		ErrUnsupported, cfg.backend)
}

// probeCapabilities reports the capabilities a factory's backend would have,
// without constructing it. Factories advertise this through a package-level
// table so that selection can skip privileged backends cheaply.
func probeCapabilities(f backendFactory) Capability {
	return factoryCaps[f.kind]
}

// factoryCaps records each backend's capabilities for pre-construction checks.
// Platform files contribute entries from their init functions.
var factoryCaps = map[Backend]Capability{}

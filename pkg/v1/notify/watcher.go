package notify

import (
	"fmt"
	"runtime"
	"sync"
)

// Watcher reports changes to the files and directories added to it.
//
// A Watcher must be created with [NewWatcher], [NewBufferedWatcher], or
// [NewWatcherWith]; the zero value is not usable. It is safe for concurrent
// use.
//
// Call [Watcher.Close] when finished. A watcher that becomes unreachable
// without being closed is eventually shut down by the garbage collector, but
// see [Watcher.Close] for why that is a safety net rather than a strategy.
type Watcher struct {
	// Events delivers filesystem changes. It is closed by [Watcher.Close],
	// which is the signal for a consuming goroutine to return.
	Events chan Event

	// Errors delivers errors that occur while watching, as distinct from
	// errors returned by [Watcher.Add]. It is closed by [Watcher.Close].
	//
	// An error here does not mean the watcher has stopped. [ErrEventOverflow]
	// in particular means events were lost but watching continues.
	Errors chan error

	// state holds everything shutdown touches.
	//
	// It is a separate allocation, and that is load-bearing rather than
	// stylistic. The cleanup that shuts an abandoned watcher down must be able
	// to reach the backend and the channels; if it reached them through the
	// Watcher, it would keep the Watcher permanently reachable and so would
	// never run. Splitting the state out lets the cleanup hold what it needs
	// while the Watcher itself remains collectable.
	state *watcherState

	// cleanup is the registration to cancel once Close has run explicitly.
	cleanup runtime.Cleanup
}

// watcherState owns every resource that must be released, independently of the
// [Watcher] that fronts it.
type watcherState struct {
	events  chan Event
	errors  chan error
	done    chan struct{}
	backend backend

	mu     sync.Mutex
	closed bool

	closeOnce sync.Once
	closeErr  error
}

// NewWatcher creates a watcher with an unbuffered Events channel.
//
// Events are delivered synchronously: the watcher cannot report a change until
// the previous one has been received. A consumer that blocks — on disk I/O, on
// a network call, on a lock — stalls the watcher and can cause the kernel's
// own queue to overflow. Hand work to another goroutine rather than doing it
// in the receive loop.
func NewWatcher() (*Watcher, error) {
	return NewWatcherWith()
}

// NewBufferedWatcher creates a watcher whose Events channel has capacity sz.
//
// Buffering absorbs bursts, which is useful when changes arrive faster than
// they can be handled but the average rate is manageable. It does not help a
// consumer that is persistently too slow; see [WithEventBuffer].
func NewBufferedWatcher(sz uint) (*Watcher, error) {
	return NewWatcherWith(WithEventBuffer(sz))
}

// NewWatcherWith creates a watcher configured by opts.
//
// It is the general form of [NewWatcher] and [NewBufferedWatcher], and the
// only way to select a backend, tune the poll interval, or set a descriptor
// budget.
func NewWatcherWith(opts ...Option) (*Watcher, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o.apply(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	st := &watcherState{
		events: make(chan Event, cfg.eventBuffer),
		errors: make(chan error),
		done:   make(chan struct{}),
	}

	b, err := newBackend(chanSink{
		events: st.events,
		errors: st.errors,
		done:   st.done,
	}, cfg)
	if err != nil {
		return nil, err
	}
	st.backend = b

	w := &Watcher{
		Events: st.events,
		Errors: st.errors,
		state:  st,
	}

	// The cleanup receives the state, never the Watcher. Passing the Watcher
	// would make it reachable from its own cleanup argument, which the runtime
	// treats as a permanent reference: the object would never be collected and
	// the cleanup would never run.
	w.cleanup = runtime.AddCleanup(w, cleanupAbandoned, st)

	return w, nil
}

// cleanupAbandoned shuts down a watcher that was dropped without being closed.
//
// Teardown runs on its own goroutine because cleanups are executed one after
// another by the runtime: blocking here would delay every other cleanup in the
// program, and stopping a backend can take as long as a scan in progress.
// Nothing can observe the intermediate state — by definition the watcher is
// already unreachable — so there is nothing to synchronise with.
func cleanupAbandoned(st *watcherState) {
	go func() {
		debugf("watcher was garbage collected without Close; shutting it down")
		_ = st.close()
	}()
}

// close performs the shutdown sequence exactly once.
//
// The ordering is the contract backends rely on:
//
//  1. Mark the watcher closed, so Add and Remove start refusing work.
//  2. Close done, releasing any backend goroutine parked mid-send. Without
//     this, a consumer that has already stopped reading would deadlock
//     shutdown.
//  3. Stop the backend, which by contract does not return until every
//     goroutine that can send has exited.
//  4. Close the channels. This is safe only because of step 3: a send on a
//     closed channel panics, so the backend must be provably silent first.
func (st *watcherState) close() error {
	st.closeOnce.Do(func() {
		st.mu.Lock()
		st.closed = true
		st.mu.Unlock()

		close(st.done)
		st.closeErr = st.backend.Close()
		close(st.events)
		close(st.errors)
	})
	return st.closeErr
}

// Add begins watching path, which may name a file or a directory.
//
// Watching a directory reports changes to the entries directly inside it, but
// not inside its subdirectories. To watch a whole tree, append "/..." to the
// path or use [Watcher.AddWith] with [WithRecursive]:
//
//	w.Add("/var/log/...")
//
// Adding a path that is already watched updates it to the new options and is
// otherwise not an error.
//
// A watch is attached to the file the path names, not to the name itself. If
// that file is renamed or replaced — as most editors do when saving — the
// watch follows the original inode and stops reporting anything useful. This
// is why watching a directory is almost always the right choice.
func (w *Watcher) Add(path string) error { return w.AddWith(path) }

// AddWith begins watching path with the given options.
//
// It fails with [ErrUnsupported] if an option asks for something the selected
// backend cannot do, rather than accepting the option and never delivering the
// events it implies.
func (w *Watcher) AddWith(path string, opts ...AddOption) error {
	// Keep the Watcher alive for the duration of the call. Without this the
	// collector may observe that the receiver is dead as soon as its state
	// pointer has been loaded, and run the cleanup while the call is still in
	// progress — turning a valid Add into a spurious ErrClosed.
	defer runtime.KeepAlive(w)

	cleaned, recursive := splitRecursive(path)

	o := defaultAddOpts()
	if recursive {
		o.recursive = true
	}
	for _, opt := range opts {
		opt.applyAdd(&o)
	}

	st := w.state
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return fmt.Errorf("notify: Add(%q): %w", path, ErrClosed)
	}

	caps := st.backend.Capabilities()
	if err := o.validate(caps); err != nil {
		return fmt.Errorf("notify: Add(%q): %w", path, err)
	}
	if o.recursive && !caps.Has(CapRecursive) {
		return fmt.Errorf("notify: Add(%q): %w: backend %s cannot watch recursively",
			path, ErrUnsupported, st.backend.Kind())
	}

	if err := st.backend.Add(cleaned, o); err != nil {
		return fmt.Errorf("notify: Add(%q): %w", path, err)
	}
	return nil
}

// Remove stops watching path.
//
// It returns [ErrNonExistentWatch] if the path is not being watched. Removing
// a recursive watch removes its descendants with it; the path may be given
// with or without the "/..." suffix.
//
// Removing a watch does not drain events already queued for it, so a consumer
// may still receive events for path after Remove returns.
func (w *Watcher) Remove(path string) error {
	defer runtime.KeepAlive(w)

	cleaned, _ := splitRecursive(path)

	st := w.state
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return fmt.Errorf("notify: Remove(%q): %w", path, ErrClosed)
	}

	if err := st.backend.Remove(cleaned); err != nil {
		return fmt.Errorf("notify: Remove(%q): %w", path, err)
	}
	return nil
}

// WatchList returns the paths currently being watched, in the form they were
// added.
//
// For a recursive watch this reports the root that was added, not every
// directory the backend watches internally to implement it.
func (w *Watcher) WatchList() []string {
	defer runtime.KeepAlive(w)

	st := w.state
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return nil
	}
	return st.backend.WatchList()
}

// Backend reports which notification mechanism this watcher is using.
//
// It never returns [BackendAuto]: by the time a watcher exists, a concrete
// backend has been chosen.
func (w *Watcher) Backend() Backend {
	defer runtime.KeepAlive(w)
	return w.state.backend.Kind()
}

// Capabilities reports what this watcher's backend can do on this host.
func (w *Watcher) Capabilities() Capability {
	defer runtime.KeepAlive(w)
	return w.state.backend.Capabilities()
}

// Close stops watching everything and closes the Events and Errors channels.
//
// It is idempotent; closing an already-closed watcher returns nil. Close does
// not wait for a consumer to drain queued events, and it will not block if the
// consumer has already stopped reading.
//
// The channels are closed only after the backend has stopped, so a consumer
// looping until Events is closed will observe every event the backend managed
// to deliver, and no send can race with the close.
//
// A watcher that becomes unreachable without being closed is shut down by the
// garbage collector, so a forgotten watcher does not leak descriptors or
// goroutines for the life of the process. Do not rely on this. Collection
// happens at a time of the runtime's choosing, which may be long after the
// resources were needed elsewhere and may be never — a process that exits
// promptly, or one that never allocates enough to trigger a collection, will
// not run it at all. Treat it as a backstop against bugs, not as a substitute
// for closing.
func (w *Watcher) Close() error {
	defer runtime.KeepAlive(w)

	// Cancel the safety net first: shutdown is about to happen explicitly, and
	// leaving the registration in place would keep the state reachable from
	// the runtime's cleanup queue for no purpose.
	w.cleanup.Stop()

	if err := w.state.close(); err != nil {
		return fmt.Errorf("notify: Close: %w", err)
	}
	return nil
}

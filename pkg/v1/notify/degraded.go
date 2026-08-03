package notify

import (
	"errors"
	"io/fs"
	"os"
	"sort"
	"sync"
	"time"
)

// A backend that spends a descriptor per watched file cannot watch every file
// in a large tree, and the descriptor budget stops it trying. The files it
// gives up on keep their creation, removal and rename events, which come from
// the directory, but lose their modification events, which do not.
//
// That loss is the one thing a watcher must not do quietly. A caller has asked
// to hear about changes and is being told about some of them, with nothing to
// distinguish the silence from a file nobody is touching.
//
// So the files that lost a descriptor are compared instead. This is the
// polling backend's method applied to a handful of paths rather than a tree:
// slower and less precise than a kernel notification, and the difference
// between reporting a change late and not reporting it at all.

// degradedState is what a file looked like when last examined. It holds the
// least that still distinguishes the changes being reported.
type degradedState struct {
	size  int64
	mtime time.Time
	mode  fs.FileMode
}

func degradedStateOf(info fs.FileInfo) degradedState {
	return degradedState{size: info.Size(), mtime: info.ModTime(), mode: info.Mode()}
}

// changed reports how s differs from prev, as a set of operations.
func (s degradedState) changed(prev degradedState) Op {
	var op Op
	if s.size != prev.size || !s.mtime.Equal(prev.mtime) {
		op |= Write
	}
	if s.mode != prev.mode {
		op |= Chmod
	}
	return op
}

// degradedPoller watches, by comparison, the files a backend could not afford
// a descriptor for.
//
// Its goroutine starts with the first file and stops when the last one goes,
// so a backend that never exhausts its budget — which is most of them, most of
// the time — pays nothing at all for this.
type degradedPoller struct {
	sink     sink
	interval time.Duration

	mu      sync.Mutex
	files   map[string]*degradedFile
	running bool
	stop    chan struct{}
	wg      sync.WaitGroup
	closed  bool
}

type degradedFile struct {
	ops   Op
	state degradedState
}

func newDegradedPoller(s sink, interval time.Duration) *degradedPoller {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return &degradedPoller{
		sink:     s,
		interval: interval,
		files:    make(map[string]*degradedFile),
	}
}

// add begins comparing path, taking its current state as the baseline so that
// what it already looks like is not reported as a change.
func (p *degradedPoller) add(path string, ops Op) {
	// Only modifications are worth comparing for. Everything else about this
	// file is still reported by the watch on its directory, and reporting it
	// twice would be worse than not reporting it here.
	ops &= Write | Chmod
	if ops == 0 {
		return
	}

	info, err := os.Lstat(path)
	if err != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}

	p.files[path] = &degradedFile{ops: ops, state: degradedStateOf(info)}
	p.startLocked()
}

// remove stops comparing path, and stops the goroutine if it was the last.
func (p *degradedPoller) remove(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.files, path)
	p.stopIfIdleLocked()
}

// watching reports how many files are being compared. It exists for Stats and
// for tests.
func (p *degradedPoller) watching() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.files)
}

// startLocked launches the goroutine if it is not already running. p.mu held.
func (p *degradedPoller) startLocked() {
	if p.running || len(p.files) == 0 {
		return
	}
	p.running = true
	p.stop = make(chan struct{})
	p.wg.Add(1)
	go p.run(p.stop)
}

// stopIfIdleLocked halts the goroutine once nothing is left to compare. p.mu
// held.
//
// Stopping matters as much as starting. A backend that briefly exceeded its
// budget and then recovered should not keep a timer running for the rest of
// the process's life.
func (p *degradedPoller) stopIfIdleLocked() {
	if !p.running || len(p.files) > 0 {
		return
	}
	close(p.stop)
	p.running = false
}

func (p *degradedPoller) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.files = map[string]*degradedFile{}
	if p.running {
		close(p.stop)
		p.running = false
	}
	p.mu.Unlock()

	p.wg.Wait()
}

func (p *degradedPoller) run(stop chan struct{}) {
	defer p.wg.Done()

	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if !p.tick() {
				return
			}
		}
	}
}

// tick compares every file once and reports what changed. It returns false
// when the watcher is shutting down.
//
// Comparing and sending both happen with the lock released: a send blocks
// until the consumer is ready, and holding the lock across one would stall
// every add and remove behind it.
func (p *degradedPoller) tick() bool {
	type change struct {
		path string
		op   Op
	}

	p.mu.Lock()
	paths := make([]string, 0, len(p.files))
	for path := range p.files {
		paths = append(paths, path)
	}
	p.mu.Unlock()
	// Sorted so that one set of changes always produces one sequence. Nothing
	// about comparison makes an order meaningful, but determinism makes the
	// behaviour testable.
	sort.Strings(paths)

	var changes []change
	for _, path := range paths {
		p.mu.Lock()
		file, ok := p.files[path]
		var prev degradedState
		var ops Op
		if ok {
			prev, ops = file.state, file.ops
		}
		p.mu.Unlock()
		if !ok {
			continue
		}

		info, err := os.Lstat(path)
		if err != nil {
			// The file is gone. Its removal is reported by the watch on the
			// directory, which is the only place that can tell a removal from
			// a rename, so nothing is emitted here — only the comparison
			// stops.
			if errors.Is(err, fs.ErrNotExist) {
				p.remove(path)
			}
			continue
		}

		next := degradedStateOf(info)
		op := next.changed(prev) & ops

		p.mu.Lock()
		if cur, still := p.files[path]; still && cur == file {
			cur.state = next
		}
		p.mu.Unlock()

		if op != 0 {
			changes = append(changes, change{path: path, op: op})
		}
	}

	for _, c := range changes {
		if !p.sink.send(Event{Name: c.path, Op: c.op}) {
			return false
		}
	}
	return true
}

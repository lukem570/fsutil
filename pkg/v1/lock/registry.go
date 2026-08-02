package lock

import (
	"os"
	"sync"
)

// Goroutines within one process are serialised here, before any of them
// competes with other processes.
//
// This layer is not merely an optimisation. Operating system locks vary in
// what they consider an owner: some are held by the open file description, so
// two descriptors in one process contend correctly, while others — POSIX
// record locks in particular — are held by the *process*, so a second
// descriptor appears to already hold the lock and acquisition wrongly
// succeeds. Worse, on those systems closing *any* descriptor to the file drops
// every lock the process holds on it, so an unrelated open and close elsewhere
// in the program silently releases a lock another goroutine believes it still
// has.
//
// Resolving contention in Go first removes that entire class of problem: at
// most one goroutine per file ever reaches the operating system, so the
// differences between those models stop mattering.
//
// Entries are keyed on file identity rather than on path, so that a symlink, a
// hard link, and a relative path that all reach the same file share one entry.
// Keying on the path would let two goroutines think they were locking
// different things.

// localLock is the process-local hold on one file.
type localLock struct {
	key  fileKey
	rw   *sync.RWMutex
	once sync.Once
}

var registry = struct {
	mu      sync.Mutex
	entries map[fileKey]*registryEntry
}{entries: make(map[fileKey]*registryEntry)}

type registryEntry struct {
	rw   sync.RWMutex
	refs int
}

// localFor returns the process-local lock for the file behind f, creating it
// if this is the first user.
func localFor(f *os.File) (*localLock, error) {
	key, err := keyOf(f)
	if err != nil {
		return nil, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	entry := registry.entries[key]
	if entry == nil {
		entry = &registryEntry{}
		registry.entries[key] = entry
	}
	entry.refs++
	return &localLock{key: key, rw: &entry.rw}, nil
}

// tryLock takes the process-local hold without waiting.
func (l *localLock) tryLock(mode Mode) bool {
	if mode == Shared {
		return l.rw.TryRLock()
	}
	return l.rw.TryLock()
}

func (l *localLock) unlock(mode Mode) {
	if mode == Shared {
		l.rw.RUnlock()
		return
	}
	l.rw.Unlock()
}

// release drops this Lock's interest in the entry, removing it once nobody is
// using it.
//
// Without the reference count the map would grow by one entry for every file
// ever locked, which for a program that locks per job is a leak that only
// shows up after a long uptime.
func (l *localLock) release() {
	l.once.Do(func() {
		registry.mu.Lock()
		defer registry.mu.Unlock()

		entry := registry.entries[l.key]
		if entry == nil {
			return
		}
		entry.refs--
		if entry.refs <= 0 {
			delete(registry.entries, l.key)
		}
	})
}

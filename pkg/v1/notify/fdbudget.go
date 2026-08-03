package notify

import (
	"fmt"
	"runtime"
	"sync"
)

// Some backends spend a file descriptor on every path they watch. That makes
// the number of watchable paths a function of the process's descriptor limit,
// which is not a property a caller of this package should have to reason
// about — and worse, it makes a watcher capable of starving the program it is
// part of. A process that has given every descriptor to its file watcher
// cannot open the files the watcher is telling it about.
//
// The budget below bounds what watching may consume, leaving the rest for the
// program. When it is exhausted, a backend does not fail: it gives up
// precision it can afford to lose — per-file descriptors, and with them
// per-file modification events — while continuing to report everything a
// directory's own descriptor can reveal. What it must never do is return
// EMFILE to a caller who asked to watch a path.

// fdReserve is the number of descriptors kept away from watching no matter
// how generous the limit is.
//
// It exists because the failure it prevents is so much worse than the one it
// causes. A watcher that runs out of budget watches a large tree slightly less
// precisely; a watcher that consumes every descriptor in the process breaks
// unrelated code in ways that surface far from here.
const fdReserve = 256

// fdMinimumBudget is the smallest budget worth having. Below this, a watcher
// is not usefully able to watch anything, so a very low limit yields a small
// budget rather than none at all.
const fdMinimumBudget = 32

// fdBudget accounts for descriptors held for watching.
type fdBudget struct {
	mu     sync.Mutex
	budget int
	held   int
	denied int
	limit  int
}

// newFDBudget derives a budget from the process limit, or uses the configured
// value if one was given.
//
// Raising the soft limit to the hard limit is attempted first. The soft limit
// is a default rather than a policy — it is frequently 1024 on systems whose
// hard limit is orders of magnitude higher — and raising it is exactly what it
// is for. Failure is not fatal; it simply means a smaller budget.
func newFDBudget(configured int) *fdBudget {
	limit := raiseFDLimit()

	budget := configured
	if budget == 0 {
		switch {
		case limit <= 0:
			// The platform will not say. Assume something modest rather than
			// unbounded: guessing high here would reintroduce exactly the
			// starvation this exists to prevent.
			budget = fdMinimumBudget * 8
		case limit <= fdReserve:
			budget = fdMinimumBudget
		default:
			budget = min(limit/2, limit-fdReserve)
			budget = max(budget, fdMinimumBudget)
		}
	}

	debugf("descriptor budget: %d (process limit %d)", budget, limit)
	return &fdBudget{budget: budget, limit: limit}
}

// acquire reserves one descriptor, reporting false if the budget is spent.
func (b *fdBudget) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.held >= b.budget {
		b.denied++
		return false
	}
	b.held++
	return true
}

// reserve records a descriptor that will be taken regardless of the budget.
//
// It is for paths the caller explicitly asked to watch. Refusing one of those
// because of an internal accounting limit would be a worse failure than the
// starvation the budget exists to prevent, so the accounting bends rather than
// the behaviour. Going over is still visible through [Stats].
func (b *fdBudget) reserve() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held++
}

// release returns one descriptor to the budget.
func (b *fdBudget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.held > 0 {
		b.held--
	}
}

func (b *fdBudget) snapshot() (held, budget, denied, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held, b.budget, b.denied, b.limit
}

// Stats describes what a watcher is currently consuming.
//
// It exists so that degradation is observable. A watcher that has run out of
// descriptor budget still works, but reports less about the files it is
// watching, and a caller has no other way to discover that.
type Stats struct {
	// Watches is the number of paths the caller has added.
	Watches int

	// Descriptors is the number of descriptors currently held for watching.
	// It is zero for backends that do not spend one per path.
	Descriptors int

	// DescriptorBudget is the most this watcher will hold. Zero means the
	// backend has no per-path descriptor cost and so needs no budget.
	DescriptorBudget int

	// DescriptorsDenied counts how many times the budget refused a
	// descriptor. A non-zero value means some watched files are being
	// reported less precisely than they would be with a higher limit — see
	// [WithFDBudget] — and is the signal to raise the process limit.
	DescriptorsDenied int

	// ProcessFDLimit is the process's own limit on open files, as discovered
	// at startup, or zero where the platform does not expose one.
	ProcessFDLimit int
}

func (s Stats) String() string {
	return fmt.Sprintf("watches=%d descriptors=%d/%d denied=%d process-limit=%d",
		s.Watches, s.Descriptors, s.DescriptorBudget, s.DescriptorsDenied, s.ProcessFDLimit)
}

// budgeter is implemented by backends that spend descriptors per path.
//
// It is a separate interface rather than a method on every backend so that
// backends with no descriptor cost are not obliged to answer a question that
// does not apply to them.
type budgeter interface {
	budgetStats() (held, budget, denied, limit int)
}

// Stats reports what this watcher is currently consuming.
//
// The field worth watching is DescriptorsDenied. On platforms where watching
// costs a descriptor per file, a non-zero value means the watcher has run out
// of budget and is reporting some files less precisely — it still sees them
// appear and disappear, but no longer sees them modified.
func (w *Watcher) Stats() Stats {
	defer runtime.KeepAlive(w)

	st := w.state
	st.mu.Lock()
	b := st.backend
	closed := st.closed
	st.mu.Unlock()

	if closed {
		return Stats{}
	}

	s := Stats{Watches: len(b.WatchList())}
	if bb, ok := b.(budgeter); ok {
		s.Descriptors, s.DescriptorBudget, s.DescriptorsDenied, s.ProcessFDLimit = bb.budgetStats()
	}
	return s
}

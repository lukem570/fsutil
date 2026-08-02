//go:build unix

package notify

import "golang.org/x/sys/unix"

// raiseFDLimit raises the soft limit on open files to the hard limit and
// returns the resulting limit, or zero if it cannot be determined.
//
// The soft limit is a default, not a policy: it is commonly 1024 on systems
// whose hard limit is a hundred times that, and raising it is precisely what a
// program that needs more descriptors is expected to do. Doing so here means a
// watcher gets a budget derived from what the system actually permits rather
// than from a historical default.
//
// Failure is not reported. A process may be forbidden from raising its own
// limit, and the only consequence is a smaller budget — which is a working
// watcher, not a broken one.
func raiseFDLimit() int {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}

	if lim.Cur < lim.Max {
		raised := lim
		raised.Cur = raised.Max
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &raised); err == nil {
			lim = raised
		}
	}

	// A limit of "infinity" is reported as a very large value. Treating it as
	// a real number would produce a budget larger than any machine can honour,
	// so it is clamped to something a process could plausibly reach.
	const sane = 1 << 20
	if lim.Cur > sane {
		return sane
	}
	return int(lim.Cur)
}

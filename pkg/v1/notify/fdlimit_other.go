//go:build !unix

package notify

// raiseFDLimit reports the process's limit on open files, or zero where the
// platform does not have one worth consulting.
//
// Windows has no per-process handle limit comparable to RLIMIT_NOFILE — the
// constraint there is kernel pool memory, which is not something a process can
// query and turn into a number. Plan 9 and the WebAssembly targets have no
// backend that spends a descriptor per watched path, so the question does not
// arise. In every case the budget falls back to a conservative default rather
// than to no limit at all.
func raiseFDLimit() int { return 0 }

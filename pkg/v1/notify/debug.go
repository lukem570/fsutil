package notify

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// debugEnvVar turns on tracing to stderr when set to any non-empty value other
// than "0".
//
// Tracing exists because filesystem notification bugs are usually reports of
// events that did not arrive, and the only way to tell "the kernel never told
// us" from "we dropped it" is to see what the kernel actually said. Backends
// trace raw kernel events before translation; the watcher traces what it
// delivers.
const debugEnvVar = "FSUTIL_DEBUG"

// debugEnabled is read once at init. Tracing is a debugging aid, not a runtime
// switch, and checking a variable is cheaper than consulting the environment
// on every event.
var debugEnabled = func() bool {
	v := os.Getenv(debugEnvVar)
	return v != "" && v != "0"
}()

var debugMu sync.Mutex

// debugStart anchors trace timestamps to process start rather than wall clock,
// so that the interesting quantity — the gap between two events — is readable
// without arithmetic.
var debugStart = time.Now()

// debugf writes a trace line to stderr when tracing is enabled.
//
// Callers on a hot path should guard with debugEnabled to avoid formatting
// arguments that will be discarded.
func debugf(format string, args ...any) {
	if !debugEnabled {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	fmt.Fprintf(os.Stderr, "%-12s notify: %s\n",
		time.Since(debugStart).Round(time.Microsecond),
		fmt.Sprintf(format, args...))
}

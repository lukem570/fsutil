// Package notify provides cross-platform filesystem notifications.
//
// A [Watcher] reports changes to files and directories through its Events
// channel. Paths are registered with [Watcher.Add] and removed with
// [Watcher.Remove]:
//
//	w, err := notify.NewWatcher()
//	if err != nil {
//		return err
//	}
//	defer w.Close()
//
//	go func() {
//		for {
//			select {
//			case ev, ok := <-w.Events:
//				if !ok {
//					return
//				}
//				if ev.Has(notify.Write) {
//					log.Printf("modified: %s", ev.Name)
//				}
//			case err, ok := <-w.Errors:
//				if !ok {
//					return
//				}
//				log.Printf("error: %s", err)
//			}
//		}
//	}()
//
//	return w.Add("/tmp")
//
// # Backends
//
// The watcher selects the best notification mechanism the host provides and
// falls back to periodic polling where none exists. [Watcher.Backend] reports
// which one is in use, and [WithBackend] requests a specific one. Requesting a
// backend the host cannot provide fails with [ErrUnsupported] rather than
// silently degrading.
//
// # Portability
//
// Filesystem notification is not uniform across operating systems, and this
// package does not pretend otherwise. Events described by [Op] constants other
// than the Unportable ones are delivered on every platform. The Unportable
// constants are delivered only where the host supports them and only when
// explicitly requested through [WithOps].
//
// Some behaviour is inherent to the platform rather than to this package:
//
//   - Watching a directory is not recursive unless [WithRecursive] is used, or
//     the path is given with a "/..." suffix.
//   - Network and virtual filesystems (NFS, SMB, FUSE, /proc, /sys) generally
//     do not generate notifications at all. The polling backend is the only
//     option there.
//   - Watching an individual file is usually a mistake: editors save by writing
//     a temporary file and renaming it over the original, which destroys the
//     watched inode. Watch the containing directory instead.
package notify

import (
	"errors"
	"fmt"
	"strings"
)

// Errors returned by [Watcher]. Test for them with [errors.Is]; they are
// wrapped with contextual detail rather than returned bare.
var (
	// ErrNonExistentWatch is returned when Remove is called on a path that is
	// not being watched.
	ErrNonExistentWatch = errors.New("notify: can't remove non-existent watch")

	// ErrClosed is returned when a method is called on a closed Watcher.
	ErrClosed = errors.New("notify: watcher already closed")

	// ErrEventOverflow is sent on the Errors channel when the kernel's event
	// queue overflowed and events were lost. The watcher remains usable, but
	// the caller should assume it has missed changes and rescan if that
	// matters.
	ErrEventOverflow = errors.New("notify: queue or buffer overflow")

	// ErrUnsupported is returned when an operation cannot be performed on this
	// platform or by the selected backend.
	ErrUnsupported = errors.New("notify: not supported on this platform")
)

// Op is a bitmask describing the filesystem operations that triggered an
// [Event]. A single event may carry more than one operation.
type Op uint32

const (
	// Create reports that a path was created in a watched directory.
	Create Op = 1 << iota

	// Write reports that a file's contents were modified.
	//
	// This is not a guarantee that the data reached disk, and a single logical
	// write may produce several events. Conversely, a program that writes with
	// mmap may produce none until the mapping is flushed.
	Write

	// Remove reports that a path was removed.
	//
	// On Unix the notification is delivered when the last link to the file is
	// gone and every descriptor to it has been closed, which may be some time
	// after the unlink call returns.
	Remove

	// Rename reports that a path was renamed. The event carries the *old*
	// name. If the destination is also watched, a separate Create event
	// reports the new name.
	Rename

	// Chmod reports a change to a path's metadata: permissions, ownership, or
	// timestamps.
	//
	// These events are frequent and rarely meaningful. Reading a file can
	// update its access time and produce one. Most programs should ignore
	// them.
	Chmod

	// UnportableOpen reports that a file was opened.
	//
	// Delivered only where the host supports it, and only when requested with
	// [WithOps]. Never delivered by default.
	UnportableOpen

	// UnportableRead reports that a file was read from.
	//
	// Delivered only where the host supports it, and only when requested with
	// [WithOps]. Never delivered by default.
	UnportableRead

	// UnportableCloseWrite reports that a file open for writing was closed.
	//
	// This is the event most programs actually want when they reach for Write:
	// it fires once, after the writer is finished, rather than once per write
	// syscall.
	//
	// Delivered only where the host supports it, and only when requested with
	// [WithOps]. Never delivered by default.
	UnportableCloseWrite

	// UnportableCloseRead reports that a file open for reading was closed.
	//
	// Delivered only where the host supports it, and only when requested with
	// [WithOps]. Never delivered by default.
	UnportableCloseRead
)

// portableOps is the set delivered on every platform, and the default mask for
// a watch added without [WithOps].
const portableOps = Create | Write | Remove | Rename | Chmod

// unportableOps is the set delivered only on request and only where supported.
const unportableOps = UnportableOpen | UnportableRead | UnportableCloseWrite | UnportableCloseRead

// allOps is every operation this package can report.
const allOps = portableOps | unportableOps

var opNames = []struct {
	op   Op
	name string
}{
	{Create, "CREATE"},
	{Write, "WRITE"},
	{Remove, "REMOVE"},
	{Rename, "RENAME"},
	{Chmod, "CHMOD"},
	{UnportableOpen, "OPEN"},
	{UnportableRead, "READ"},
	{UnportableCloseWrite, "CLOSE_WRITE"},
	{UnportableCloseRead, "CLOSE_READ"},
}

// Has reports whether o contains h. It is true if any bit of h is set in o.
func (o Op) Has(h Op) bool { return o&h != 0 }

// String returns a human-readable form such as "CREATE|WRITE".
func (o Op) String() string {
	if o == 0 {
		return "[no events]"
	}

	var b strings.Builder
	for _, n := range opNames {
		if o&n.op != 0 {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(n.name)
		}
	}

	if unknown := o &^ allOps; unknown != 0 {
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "0x%x", uint32(unknown))
	}
	return b.String()
}

// Event reports a change to a single path.
type Event struct {
	// Name is the path of the file or directory that changed, in the same form
	// it was passed to [Watcher.Add]: a watch added with a relative path
	// produces relative names.
	Name string

	// Op is the set of operations that triggered this event.
	Op Op
}

// Has reports whether the event includes op.
func (e Event) Has(op Op) bool { return e.Op.Has(op) }

// String returns a human-readable form such as `CREATE "/tmp/file"`.
func (e Event) String() string {
	return fmt.Sprintf("%-13s %q", e.Op.String(), e.Name)
}

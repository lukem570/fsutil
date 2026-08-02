# Backends

A backend is the mechanism by which changes reach the program. One is chosen
automatically — the best the host provides, falling back to polling — and
`notify.Backends()` reports what was available.

The differences between them are not implementation details. They determine
which events you can see at all, what watching costs in kernel resources, and
how a large tree behaves. This page describes each one on its own terms.

## Capabilities

Backends advertise what they can do, and `Watcher.AddWith` refuses options the
chosen backend cannot honour rather than accepting them and doing nothing.

| Capability | Meaning |
|---|---|
| `CapRecursive` | Watches a tree natively, so recursion costs no extra resources |
| `CapUnportableOps` | Can report opens, reads, and closes |
| `CapNoFollow` | Can watch a symlink itself rather than its target |
| `CapPrivileged` | Needs elevation, so is never chosen automatically |
| `CapFDPerPath` | Consumes a descriptor per watched **path** |
| `CapFDPerDir` | Consumes a kernel resource per watched **directory** |
| `CapPreciseRename` | Can tell a rename from a delete followed by a create |

The distinction between `CapFDPerPath` and `CapFDPerDir` is the one that
decides whether watching a large tree is cheap or ruinous.

## inotify — Linux

One kernel descriptor for the whole watcher, plus one *watch descriptor* per
watched directory. Watch descriptors are a finite per-user resource governed by
`fs.inotify.max_user_watches`, which is the single most common production
failure: a recursive watch on a large tree exhausts it, and the raw error is
`ENOSPC`, which does not mean the disk is full and gives no hint what to do.
This package translates it and names the sysctl.

inotify reports opens, reads, and closes, so it is the reference implementation
for the `Unportable` operations. `UnportableCloseWrite` is the one most programs
actually want when they reach for `Write`: it fires once, after the writer has
finished, rather than once per write syscall.

**It watches inodes, not names.** A watch follows the file a path referred to
when the watch was added. If that file is replaced — which is what an editor
does when it saves — the watch stays with the file that was moved aside. Watch
the containing directory instead.

Recursion is not native. It is provided by the shared wrapper described below.

## kqueue — macOS and the BSDs

Watching anything requires holding a **descriptor open for it**. That is the
defining property: watch count competes directly with the process's own limit
on open files, governed by `kern.maxfiles` and `kern.maxfilesperproc`.

This package reduces the cost where it can. A directory's own descriptor
already reveals that an entry appeared or vanished, so per-file descriptors are
opened **only when a watch asks for `Write` or `Chmod`**. A watch interested
only in creation, removal, and renaming costs one descriptor per directory
however many files it contains.

kqueue does not say *what* changed inside a directory, only that something did,
so listings are compared to find out. It also cannot pair a rename: a file
renamed within a directory looks exactly like one name vanishing and another
appearing. Both halves are recovered here — the vanished file's own descriptor
reports whether it was renamed or deleted, and file identity is matched across
the two names.

It cannot report opens, reads, or closes at all.

## poll — everywhere

Rescans watched paths on an interval and compares the result with the previous
scan. It is the universal fallback and the only backend that works on network
and virtual filesystems, because it depends on nothing but the ability to stat.

Its limitations follow from how it works and cannot be engineered away:

- Latency is up to one interval.
- Changes that cancel out within one interval are invisible. A file created and
  deleted between two scans is never reported.
- A modification that changes neither size nor modification time is invisible.
  Rare with nanosecond timestamps, real on filesystems with one-second
  granularity.
- Cost is one `lstat` per watched path per interval.

It cannot report opens, reads, or closes, since they leave no trace in the
metadata it compares.

Polling is not a lesser mode to be avoided. On an NFS mount it is the correct
answer, and the only one.

## Rejected: FSEvents on macOS

FSEvents requires CoreFoundation, and therefore cgo. This module is pure Go, so
it is out of scope — not deferred, but excluded. macOS is served by kqueue and
polling.

This is recorded here so the decision is not re-litigated. Reversing it would
mean giving up `CGO_ENABLED=0` builds and clean cross-compilation, which is a
larger cost than the benefit FSEvents would bring over kqueue.

## Planned

- **fanotify** (Linux 5.9+) — can watch an entire mount or filesystem with one
  mark, making recursion free, but requires `CAP_SYS_ADMIN`. It will never be
  chosen automatically: being usable only because a process happens to be
  running as root is not a property to depend on silently.
- **ReadDirectoryChangesW** (Windows) — watches a tree natively.
- **USN journal** (Windows) — observes a whole volume without per-directory
  handles and survives buffer overflow, but requires elevation.
- **FEN** (illumos, Solaris).

## Recursion

Most kernel interfaces watch one directory at a time. Extending that to a tree
— placing watches on subdirectories, adopting them as they appear, pruning them
as they go — does not depend on which interface delivered the event, so it is
implemented once and layered over any backend lacking `CapRecursive`.

The case that makes this hard is worth understanding, because it determines
whether a recursive watcher is trustworthy. Rename a populated directory into a
watched tree and the kernel reports **one** directory appearing, saying nothing
about the files that came with it. Those files existed before any watch could
cover them, so they will never be reported. Adoption therefore places the watch
*and walks* what is already there.

That fix creates the opposite problem: a file created just as the watch goes on
can be found by the walk *and* reported by the kernel. Walked paths are
recorded briefly and the first matching kernel event for each is dropped, so
each creation is reported once.

A backend with native recursion skips the wrapper entirely.

# Platform notes

Behaviour that comes from the operating system rather than from this package.
None of it can be abstracted away, so it is documented instead.

## Resource limits

### Linux

```sh
sysctl fs.inotify.max_user_watches    # watches per user, often 8192 or 65536
sysctl fs.inotify.max_user_instances  # watchers per user, often 128
sysctl fs.inotify.max_queued_events   # queued events per watcher, often 16384
```

A recursive watch consumes one watch per **directory**, so a tree with more
directories than `max_user_watches` cannot be watched entirely. The kernel
reports `ENOSPC`, which is among the least helpful errors in Linux: it does not
mean the disk is full, and the fix is a sysctl most people have never met. This
package translates it and names the limit.

The limits are per **user**, not per process. An editor, a language server, and
a file sync daemon all draw on the same budget, so exhausting it breaks tools
that have nothing to do with the program that used it up.

Raise them persistently in `/etc/sysctl.d/`:

```
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
```

Exceeding `max_queued_events` produces `ErrEventOverflow` on the `Errors`
channel. Watching continues, but events were lost: the program's picture of the
tree is now incomplete and only a rescan can repair it.

### macOS and the BSDs

```sh
sysctl kern.maxfiles          # system-wide open files
sysctl kern.maxfilesperproc   # per process
ulimit -n                     # the shell's own limit, usually lower than both
```

Every watched path costs an open descriptor, so watching competes with the
program's own file I/O. `ulimit -n` is frequently the real constraint and is
often much lower than the kernel limits.

This package opens per-file descriptors only when a watch asks for `Write` or
`Chmod`; see [backends.md](backends.md).

### Containers

Container runtimes commonly set a low `RLIMIT_NOFILE` and inherit the host's
inotify limits, which are shared with every other container on the machine. A
watcher that works on a developer's machine and fails in production usually
fails here.

## Filesystems that report nothing

NFS, SMB, FUSE (including sshfs and most cloud-storage mounts), `/proc`, and
`/sys` generally deliver no notifications at all. The kernel has nothing to
report, because the change happened somewhere else.

There is no fix, only the polling backend:

```go
w, _ := notify.NewWatcherWith(
    notify.WithBackend(notify.BackendPoll),
    notify.WithPollInterval(2*time.Second),
)
```

Polling here is not a workaround; it is the correct answer.

## Editors and atomic saves

Most editors do not write to the file you are editing. They write a temporary
file and rename it over the original, so that a crash mid-save cannot leave a
half-written file.

The consequence for a watcher is that **the file you were watching no longer
exists**. A kernel watch follows the inode, which has been moved aside, so it
reports nothing about the new file now wearing that name.

Watch the containing directory. A save then appears as a `Create` — or a
`Rename` followed by a `Create` — for the name you care about, which is what
you actually wanted to know.

## Event semantics

- **An event is a hint, not a description of the present.** By the time you
  receive a `Create`, the file may be gone. Go and look; do not assume.
- **One save may produce several `Write` events.** Debounce if you are
  triggering expensive work.
- **A program using `mmap` may produce no `Write` events** until it flushes.
- **`Chmod` is frequent and rarely meaningful.** Reading a file can update its
  access time and produce one.
- **Ordering between different paths is not guaranteed** across backends.
- **A `Rename` reports the name that was vacated.** If the destination is also
  watched, a separate `Create` reports the new name.

## Watching a single file

It usually does not do what people expect, for the reason above: the watch
follows the file, not the name. It is supported, and it is right when the file
is genuinely appended to in place — a log file, say — and wrong for anything an
editor or a deployment process might replace.

## Case-insensitive filesystems

APFS and NTFS are case-insensitive by default. `Foo.txt` and `foo.txt` are one
file, but the events report whichever spelling the change used, which may not
match what was passed to `Add`. Compare paths case-insensitively on those
systems, or normalise before comparing.

## File locks

**Advisory on Unix, mandatory on Windows.** On Unix a lock coordinates
processes that ask, and does nothing to a process that simply opens the file
and writes. On Windows the lock is enforced: an unrelated process's read or
write inside a locked range fails.

This is a behavioural difference rather than a stricter shade of the same
thing. Code that works on Unix because a non-participating writer is ignored
will see that writer's calls fail on Windows. Write as though every participant
cooperates, because on Unix it must.

`lock.Mechanism()` and `lock.IsMandatory()` report which applies at runtime.

### Locking over a network filesystem

Do not. NFS, SMB, and FUSE locking ranges from reliable to entirely absent
depending on the server, the client, the mount options, and the protocol
version, and none of that is visible from the program: taking a lock appears to
succeed, and two machines may hold the same exclusive lock at once.

If processes on different machines must coordinate, use something built for
that.

### Crash safety

A lock held by a process that dies is released by the operating system, so a
crash does not leave a file locked forever.

The exception is Plan 9 and the WebAssembly targets, which have no locking
syscall and fall back to creating a file exclusively. That cannot survive its
holder dying, and there is no sound automatic remedy — a guard file left by a
crash and one held by a busy process are indistinguishable — so the file is
left for a human to remove.

# fsutil

Filesystem notifications and advisory file locks for Go. Pure Go, no cgo, no
dependencies beyond `golang.org/x/sys`.

```go
import (
    "github.com/lukem570/fsutil/pkg/v1/notify"
    "github.com/lukem570/fsutil/pkg/v1/lock"
)
```

| Package | What it does |
|---|---|
| [`pkg/v1/notify`](pkg/v1/notify) | Watch files and directories for changes, recursively if asked |
| [`pkg/v1/lock`](pkg/v1/lock) | Advisory file locks that coordinate between processes |

## Install

```sh
go get github.com/lukem570/fsutil
```

Requires Go 1.24 or newer.

## Watching

```go
w, err := notify.NewWatcher()
if err != nil {
    return err
}
defer w.Close()

go func() {
    for {
        select {
        case ev, ok := <-w.Events:
            if !ok {
                return
            }
            if ev.Has(notify.Write) {
                log.Printf("modified: %s", ev.Name)
            }
        case err, ok := <-w.Errors:
            if !ok {
                return
            }
            log.Printf("watch error: %s", err)
        }
    }
}()

return w.Add("/var/log")
```

Watching a directory reports changes to the entries directly inside it. To
watch a whole tree, add a `/...` suffix — the same notation Go uses for package
patterns:

```go
w.Add("/var/log/...")
```

Recursion works on every platform, including those whose kernel interface has
no notion of it.

### Choosing what to hear about

```go
// Only tell me when a file is finished being written, not on every write.
w.AddWith("/srv/incoming", notify.WithOps(notify.UnportableCloseWrite))
```

Operations named `Unportable` are those only some kernels can report. They are
never delivered by default, and asking for one on a backend that cannot produce
it fails with `ErrUnsupported` — rather than silently never firing, which is
the worst possible outcome because it looks like nothing happened.

### Choosing a backend

A backend is selected automatically: the best mechanism the host provides,
falling back to polling. You can inspect and override that.

```go
notify.Backends()                                    // what this host offers
w, err := notify.NewWatcherWith(notify.WithBackend(notify.BackendPoll))
w.Backend()                                          // what was chosen
```

Naming a backend the host cannot provide is an error, not an invitation to
substitute something slower. That matters when the choice is load-bearing —
watching an NFS mount, say, where only polling works at all.

## Locking

```go
l, err := lock.New("/var/lib/thing/.lock")
if err != nil {
    return err
}
defer l.Close()

if err := l.Lock(ctx); err != nil {
    return err
}
defer l.Unlock()
```

`RLock` takes a shared lock instead, which any number of holders may hold at
once but which excludes every exclusive holder. `TryLock` and `TryRLock` return
immediately, reporting whether they succeeded.

Waiting honours the context. That is a design choice with a cost worth knowing
about: acquisition retries with backoff rather than blocking in the kernel,
because a goroutine parked in a blocking lock syscall cannot be interrupted by
a context. Cancellation therefore works, at the price of up to one retry
interval of latency.

## Backends

| Backend | Platform | Mechanism |
|---|---|---|
| `inotify` | Linux | `inotify_init1` |
| `kqueue` | macOS, FreeBSD, OpenBSD, NetBSD, DragonFly | `EVFILT_VNODE` |
| `poll` | everywhere | periodic rescan |
| `fanotify` | Linux 5.9+ | *planned* |
| `directory-changes` | Windows | *planned* |
| `usn-journal` | Windows | *planned* |
| `fen` | illumos, Solaris | *planned* |

Platforms without a native backend yet still work — they use polling, which is
slower but correct. See [docs/backends.md](docs/backends.md) for what each one
can and cannot do.

FSEvents on macOS is deliberately absent: it requires CoreFoundation, and
therefore cgo. macOS is served by kqueue and polling.

## Limitations worth knowing before you start

Most surprises with filesystem notification come from the operating system
rather than from any library, so they are worth stating plainly.

- **Watch directories, not files.** A watch attaches to the file a path names,
  not to the name. Editors save by writing a temporary file and renaming it
  over the original, which destroys the watched file — so a watch on a file you
  are editing stops reporting anything useful after the first save.
- **Events describe what happened, not what is true now.** By the time you
  receive a `Create`, the file may already be gone. Treat an event as a hint to
  go and look, not as a description of the current state.
- **A `Write` is not a completed write.** One logical save may produce several,
  and a program using `mmap` may produce none until it flushes.
- **`Chmod` is noisy.** Reading a file can update its access time and produce
  one. Most programs should ignore them.
- **Network and virtual filesystems report nothing.** NFS, SMB, FUSE, `/proc`
  and `/sys` generally deliver no events at all. Polling is the only option
  there, and it is not a workaround but the correct answer.
- **Kernel resources are finite.** Watching a large tree can exhaust
  `fs.inotify.max_user_watches` on Linux or the open-file limit on the BSDs.
  Errors for these name the limit and how to raise it.
- **File locks are advisory on Unix and mandatory on Windows.** A
  non-participating writer is ignored on one and fails on the other. Write code
  that assumes every participant cooperates, because on Unix it must.
- **Do not lock over a network filesystem.** NFS and SMB locking ranges from
  reliable to entirely absent depending on server, client, mount options, and
  protocol version, and none of that is visible from the program.

[docs/platform-notes.md](docs/platform-notes.md) has the details.

## Command line

A small tool comes with the module, mostly so that a bug report can be reduced
to a command:

```sh
go build -o bin/fsutil ./cmd/fsutil

bin/fsutil backends                    # what this host offers
bin/fsutil watch -recursive ./dir      # print events as they happen
bin/fsutil lock /tmp/x.lock -- make    # run a command holding a lock
```

Set `FSUTIL_DEBUG=1` to trace raw kernel events to stderr. When an expected
event does not arrive, that trace is what distinguishes "the kernel never told
us" from "we dropped it".

## Development

The build system is [Task](https://taskfile.dev):

```sh
task check        # fmt, vet, lint, race tests, cross-compile — the full gate
task test:race
task crossbuild   # every supported GOOS/GOARCH, with cgo disabled
task deps:verify  # fail if anything under pkg/ imports outside stdlib + x/sys
```

Every supported operating system runs the real test suite on a real instance of
itself in CI — hosted runners where they exist, virtual machines for the BSDs
and illumos where they do not. Cross-compilation covers the rest. A
notification library tested only on Linux is untested.

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

MIT. See [LICENSE](LICENSE).

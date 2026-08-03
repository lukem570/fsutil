# Locking

A guide to `pkg/v1/lock`. The API reference is on
[pkg.go.dev](https://pkg.go.dev/github.com/lukem570/fsutil/pkg/v1/lock); this
covers the parts that are about file locks in general rather than about this
package.

## What a file lock is for

Coordinating processes that have nothing else in common but a filesystem. A
package manager and a build tool that must not run at once; several copies of a
daemon where only one should be active; a migration that must happen once.

Within one process, use `sync.Mutex`. A file lock is slower, can fail, and buys
nothing there.

## Advisory, and what that means

On Unix these locks are **advisory**. They coordinate processes that ask, and do
absolutely nothing to a process that opens the file and writes. Nothing is
enforced; the lock is a convention.

On Windows they are **mandatory**. The operating system fails an unrelated
process's read or write inside a locked range.

This is a behavioural difference rather than a stricter shade of the same
thing, and it runs the surprising way round: code that works on Unix *because*
a non-participating writer is ignored will see that writer's calls fail on
Windows. Write as though every participant cooperates, because on Unix it must.

`lock.Mechanism()` and `lock.IsMandatory()` report which applies at runtime.

## Waiting

```go
ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()

if err := l.Lock(ctx); err != nil {
    return err
}
defer l.Unlock()
```

Waiting **retries with backoff rather than blocking in the kernel**. That costs
up to one retry interval of latency, and buys cancellation that works: a
goroutine parked in a blocking `flock` cannot be interrupted by a context, so a
caller who cancelled would go on waiting anyway while the goroutine leaked.

Tune it with `WithRetryInterval` and `WithMaxRetryInterval`, or bound every
acquisition with `WithTimeout`.

`TryLock` and `TryRLock` do not wait at all. A `false` return means somebody
else holds it — an ordinary outcome, not an error.

## Shared and exclusive

`RLock` takes a shared lock: any number of holders at once, but no exclusive
holder alongside them. The usual reader/writer arrangement, extended across
process boundaries.

Plan 9 and the WebAssembly targets have no locking syscall and fall back to
exclusive file creation, which cannot express "several holders, no writer", so
a shared lock is granted as an exclusive one. Granting something stricter than
asked for costs concurrency and never correctness.

## What happens when a holder dies

The operating system releases it. A process killed mid-hold does not leave the
file locked, which is what makes file locks usable for anything that matters —
a lock needing manual clearing after every crash gets cleared by a script that
guesses, and then it is not a lock.

The exception is the exclusive-create fallback above. It cannot survive its
holder dying, and there is no sound automatic remedy: a guard file left by a
crash and one held by a busy process are indistinguishable. The file is left
for a person to remove.

## Diagnosing a wait

```go
l, _ := lock.New(path, lock.WithHolderInfo())
```

The holder records its process id, host, and start time, so a program that
cannot acquire can say *who* has it:

```go
if h, err := l.Holder(); err == nil && h.PID != 0 {
    log.Printf("waiting for pid %d on %s since %s", h.PID, h.Hostname, h.Since)
}
```

"Waiting for the lock" is a support ticket; "waiting for pid 4123 on build-07"
is an answer.

**The file's contents never decide whether a lock is held.** They may name a
process that exited seconds ago, or a holder that never enabled recording at
all. The only sound way to learn whether a lock is held is to try to take it.

## Do not lock over a network filesystem

NFS, SMB, and FUSE locking ranges from reliable to entirely absent depending on
the server, the client, the mount options, and the protocol version — and none
of that is visible from the program. Taking the lock appears to succeed, and
two machines may hold the same exclusive lock at once.

If processes on different machines must coordinate, use something built for it.

## Two things that bite

**Close what you create.** A `Lock` that becomes unreachable without being
closed is released by the garbage collector, but that is a backstop for bugs,
not a plan: collection happens whenever the runtime decides, possibly long
after another process began waiting, and possibly never in a program that exits
promptly. An abandoned lock is not a slow leak — it is a hang somewhere else.

**Do not delete the lock file.** Removing it while another process has it open
lets a third create a new one and take a lock the second still believes it
holds. This package never removes it, and neither should you; an empty file is
not worth the race.

## Why there is no `sync`-shaped wrapper

A `Mutex` with `Lock()` and `Unlock()` taking no arguments and returning no
errors would be familiar, and that is the problem. A file lock fails in ways
`sync.Mutex` cannot — the filesystem refuses, the file vanishes, a network
mount lies — and an API of that shape must hide those or panic. Code written
against it would look correct while ignoring failures that matter.

The context-taking API is slightly less convenient and does not invite the
mistake.

# Contributing

## Getting set up

```sh
task tools:install   # gofumpt, golangci-lint
task check           # fmt, vet, lint, race tests, cross-compile
```

`task check` is the gate. If it passes, the change is in reasonable shape.

## Rules that are not negotiable

**No cgo, ever.** Library code imports the standard library and
`golang.org/x/sys` and nothing else. This is enforced mechanically by a
`depguard` rule and independently by `task deps:verify`, so a forbidden import
fails the build rather than being noticed in review.

The consequence worth knowing: mechanisms that need a C library are out of
scope, not merely unimplemented. macOS FSEvents is the notable one.

**Every backend file carries an explicit build tag.** Platform differences live
in separate files, never in `if runtime.GOOS ==` branches, so that
cross-compilation stays honest.

**Missing syscall definitions are declared locally.** If `golang.org/x/sys`
lacks a constant or struct, declare it in the backend file with a comment
citing the OS header it came from. Do not vendor or patch `x/sys`.

## Adding a backend

1. Implement the internal `backend` interface: `Add`, `Remove`, `WatchList`,
   `Close`, `Kind`, `Capabilities`.
2. Register it from `init` with a priority and a runtime availability probe.
   Compiling for a platform does not mean the mechanism works there — a sandbox
   may forbid the syscall, or a limit may already be exhausted.
3. Declare capabilities honestly. A backend that claims something it cannot do
   produces watches that silently never fire, which is the worst failure mode
   available.
4. Wire it into the conformance suite. Do not write a parallel set of tests.

Two contract points are easy to miss and cause rare, hard-to-diagnose failures:

- **`Close` must not return while any goroutine can still deliver.** The caller
  closes the event channels immediately afterwards, and a send on a closed
  channel panics.
- **Never hold a lock while delivering.** Delivery can block on a consumer, and
  a layered sink may call back into the backend — the recursive wrapper adds
  watches in response to events.

## Testing

**The conformance suite is the definition of correct behaviour.** Every backend
runs every test in it. The value of a cross-platform library is that a program
behaves the same everywhere, and the only way to know that is to hold each
implementation to one shared description rather than to tests written to match
whatever it already did.

Where a backend genuinely cannot comply, it says so through a capability and
the test skips. A skip is a documented platform difference, visible in the
output — not a gap nobody noticed.

**Never use a bare `time.Sleep` to wait for an event.** Use the deadline-based
helpers. A sleep either makes the suite slow or makes it flaky, usually both.

**Verify tests that could pass vacuously.** Anything depending on garbage
collection, on a second process, or on a race is capable of passing for the
wrong reason. Break the mechanism deliberately and confirm the test fails.
Several tests here were checked that way and the practice has already caught
tests that proved nothing. A test that cannot fail is worse than no test,
because it is trusted.

**Filesystem tests must survive `task test:stress`** (`-race -count=20`). Use
`t.TempDir()`, never a fixed path.

## Style

Match the surrounding code. Comments should explain *why* — a reader can see
what the code does. The comments worth writing are the ones recording a
constraint that is not visible locally: why a descriptor is deregistered before
closing, why a lock is taken in a particular order, why an event is suppressed.

## Commits

Explain the reasoning, not the diff. A reader in a year needs to know why the
change was made and what alternatives were rejected; `git show` already tells
them what changed.

## Reporting a bug

Filesystem behaviour varies enormously by platform, so a report needs the
operating system and version, the kernel version on Linux, the **filesystem**
(ext4, APFS, NFS, overlayfs — this is frequently the whole answer), and the
backend in use, which `fsutil backends` prints.

Reduce it to a command if you can:

```sh
FSUTIL_DEBUG=1 fsutil watch -recursive ./dir
```

The debug trace shows raw kernel events before translation, which is what
distinguishes "the kernel never told us" from "we dropped it".

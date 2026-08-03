# Changelog

Notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-03

First release. Every backend below passes the conformance suite on a real
machine in CI — Linux (amd64 and arm64), macOS (Intel and Apple Silicon),
Windows (amd64 and arm64), FreeBSD, OpenBSD, NetBSD and illumos.

The version is deliberately not 1.0.0. In Go that is a permanent commitment to
an API that no external program has yet exercised, and the options surface may
still change as the remaining backends land. Nothing here is expected to break;
the number says the promise has not been made rather than that the code is
unfinished.

### Added

- `pkg/v1/notify`: watcher API — `Watcher`, `Event`, `Op`, `Add`, `AddWith`,
  `Remove`, `Close`, `WatchList`, `Backend`, `Capabilities`.
- `pkg/v1/notify`: options `WithBackend`, `WithPollInterval`, `WithEventBuffer`,
  `WithFDBudget`, `WithOps`, `WithNoFollow`, `WithRecursive`, `WithBufferSize`.
- `pkg/v1/notify`: operations that only some kernels report —
  `UnportableOpen`, `UnportableRead`, `UnportableCloseWrite`,
  `UnportableCloseRead`. Never delivered by default; requesting one from a
  backend that cannot produce it fails rather than silently never firing.
- `pkg/v1/notify`: polling backend, available on every supported platform and
  the only option on network and virtual filesystems.
- `pkg/v1/notify`: inotify backend for Linux.
- `pkg/v1/notify`: fanotify backend for Linux 5.9 and newer. Never selected
  automatically, since it requires CAP_SYS_ADMIN and a backend that works only
  because a process happens to be privileged is not one to depend on silently.
  Paths are resolved by matching recorded directory handles rather than through
  `open_by_handle_at`, which avoids needing a second capability.
- `pkg/v1/notify`: Windows backend using ReadDirectoryChangesW, with native
  subtree watching, so a recursive watch costs one handle for a whole tree.
- `pkg/v1/notify`: `WithExclude`, which prunes matching directories from a
  recursive watch entirely rather than filtering their events, so watching a
  repository need not spend kernel watches on its version-control directory.
- `pkg/v1/notify`: a descriptor budget, so the number of watchable paths is not
  bounded by the process limit on open files and a watcher cannot starve the
  program it belongs to. Files it cannot afford a descriptor for are compared
  on an interval instead, so exhausting the budget costs latency rather than
  silence, and `Watcher.Stats()` reports when that is happening.
- `pkg/v1/notify`: kqueue backend for macOS and the BSDs. Per-file descriptors
  are opened only when a watch asks for `Write` or `Chmod`, so a watch
  interested only in creation and removal costs one descriptor per directory.
- `pkg/v1/notify`: recursive watching, via `Add("dir/...")` or
  `WithRecursive()`, layered over any backend without native support. A
  directory populated before it could be watched — a tree renamed in
  atomically, for instance — is walked on adoption, so its contents are
  reported rather than silently missed.
- `pkg/v1/notify`: a watcher abandoned without `Close` is shut down by the
  garbage collector, so a forgotten watcher does not leak descriptors and
  goroutines for the life of the process.
- `pkg/v1/lock`: advisory file locks — `Lock`, `TryLock`, `RLock`, `TryRLock`,
  `Unlock`, `Close`, `Holder`. `flock` on Unix, `LockFileEx` on Windows,
  `fcntl` on AIX, exclusive-create elsewhere.
- `pkg/v1/lock`: `Mechanism` and `IsMandatory`, since Unix locks are advisory
  and Windows locks are mandatory — a behavioural difference, not a stricter
  shade of the same thing.
- `cmd/fsutil`: `watch`, `lock`, and `backends` subcommands, mainly so a bug
  report can be reduced to a command.
- `FSUTIL_DEBUG=1` traces raw kernel events to stderr.
- CI runs the real test suite on real instances of every supported operating
  system, with virtual machines for those that have no hosted runner.

### Testing

- One conformance suite is run against every backend available on the host, so
  each implementation is held to a single shared description of correct
  behaviour rather than to tests written to match what it already did. Where a
  backend genuinely cannot comply it says so through a capability and the test
  skips, making platform differences visible in the output.
- Stress and resource tests for the failures that need volume, churn or timing
  to appear: descriptor leaks under repeated add/remove, four hundred
  simultaneous watches, removal while events are in flight, shutdown against a
  moving target.
- Locking is tested between real processes by re-executing the test binary,
  including the case that makes file locks usable at all — that the operating
  system releases one whose holder was killed.
- A differential test runs the polling backend as an oracle alongside the
  native one on the same tree. Polling decides what happened by looking rather
  than by being told, so a durable change it observed and the native backend
  did not is a defect in the latter.
- Fuzzers cover the path handling. The first run found an ambiguity rather than
  a crash: "..." is a legal filename, so a trailing marker cannot be
  distinguished from a directory named after it.
- Benchmarks cover both packages, and have already found two costs invisible on
  reading — a lock retry timer built before any attempt was made, and a
  relative path derived on the hottest exclusion path.
- Every test depending on garbage collection, a second process, or a race is
  verified by negative control: the mechanism is deliberately broken and the
  test confirmed to fail. A test that cannot fail is worse than none, because
  it is trusted.

### Known gaps

- The kqueue and Windows backends compile and vet for every target they claim,
  but have not yet run on one — there was no host available while they were
  written. CI on real machines exists to close that gap. Treat them as
  unverified until it has.
- illumos and Solaris have no native backend and fall back to polling.
- Where a backend runs out of descriptor budget, affected files are compared on
  an interval rather than watched, so modifications arrive up to one interval
  late and one undone within an interval is not seen. Every operation is still
  reported. See `Watcher.Stats().DescriptorsDenied`.

[unreleased]: https://github.com/lukem570/fsutil/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/lukem570/fsutil/releases/tag/v0.1.0

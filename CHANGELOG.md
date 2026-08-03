# Changelog

Notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Pre-release. The API is settling but not yet frozen.

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
  program it belongs to. `Watcher.Stats()` makes any resulting loss of
  precision observable.
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

### Known gaps

- The kqueue and Windows backends compile and vet for every target they claim,
  but have not yet run on one — there was no host available while they were
  written. CI on real machines exists to close that gap. Treat them as
  unverified until it has.
- illumos and Solaris have no native backend and fall back to polling.
- Where a backend runs out of descriptor budget, affected files keep their
  creation, removal and rename events but lose modification events. See
  `Watcher.Stats().DescriptorsDenied`.

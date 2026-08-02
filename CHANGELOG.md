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

- Windows and illumos have no native backend yet and fall back to polling.
- The kqueue backend compiles and vets on all six BSD targets but has not yet
  run on one; CI exists to close that gap.
- Two recursive watches sharing a subdirectory: removing one releases the
  shared inner watch.

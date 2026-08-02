# Security

## Reporting a vulnerability

Report privately through GitHub's [security advisory][advisory] form rather
than as a public issue.

[advisory]: https://github.com/lukem570/fsutil/security/advisories/new

Please include what an attacker can achieve, how to reproduce it, and the
platform and filesystem involved — the last matters more here than in most
projects, since much of this code is platform-specific.

## What is in scope

This module watches paths and takes locks on behalf of a program that chooses
both. The security-relevant surface is small but real:

- **Escaping a watched tree.** A recursive watch must stay within its root. A
  symlink inside the tree, or a directory swapped for a symlink while the tree
  is being walked, must not cause watches to be placed elsewhere. Walks are
  confined with `os.Root` for this reason; a way around that confinement is a
  vulnerability.
- **Resource exhaustion by a third party.** Watching a tree an attacker can
  write to means they influence how many kernel watches are consumed. Being
  able to exhaust a per-user limit and so break unrelated programs is in scope.
- **Locks that do not exclude.** Two holders of the same exclusive lock on one
  machine is a defect with security consequences for whatever the lock protects.
- **Descriptor confusion.** Attributing an event to the wrong path — through
  descriptor reuse, for instance — could mislead a program into acting on a
  file that did not change.

## What is not

- **Advisory locks not stopping a non-participating writer on Unix.** That is
  what advisory means. See [docs/platform-notes.md](docs/platform-notes.md).
- **Locking being unreliable over NFS, SMB, or FUSE.** A property of those
  filesystems, documented rather than fixed.
- **Missed events on filesystems that deliver none**, such as `/proc` or a
  network mount. Use the polling backend.
- **Time-of-check to time-of-use races in calling code.** An event says
  something happened, not that it is still true. A program that treats an event
  as a guarantee about the present has a race of its own making.
- **Watching a path a program was told to watch.** Deciding what is safe to
  watch is the caller's.

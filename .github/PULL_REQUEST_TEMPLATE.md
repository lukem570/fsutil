## What this changes, and why

<!-- The reasoning, not the diff — `git show` already covers what changed.
     What problem does this solve, and what alternatives were rejected? -->

## Checklist

- [ ] `task check` passes (fmt, vet, lint, race tests, cross-compile)
- [ ] New behaviour is covered by the conformance suite rather than by tests
      written for one backend
- [ ] Any test that depends on garbage collection, a second process, or a race
      was verified by **negative control** — break the mechanism deliberately
      and confirm the test fails. A test that cannot fail is worse than none,
      because it is trusted.
- [ ] No new dependency outside the standard library and `golang.org/x/sys`
- [ ] Platform differences that cannot be hidden are recorded in
      `docs/deviations.md`

## Platforms

<!-- Which did you test on? CI covers Linux, macOS, Windows, FreeBSD, OpenBSD
     and NetBSD, so "CI" is a complete answer for anything it runs. -->

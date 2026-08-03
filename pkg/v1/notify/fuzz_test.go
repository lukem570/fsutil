package notify

import (
	"path/filepath"
	"strings"
	"testing"
)

// Paths reach this package from callers, from configuration files, and from
// the kernel, so they arrive in shapes nobody designed for: trailing
// separators, repeated dots, unicode, embedded newlines, and on Windows a
// mixture of both separators. None of it should be able to panic, and the two
// pieces of path handling here have properties worth stating rather than
// merely exercising.

// FuzzSplitRecursive checks the parsing of the "/..." suffix.
//
// The properties are what matter: the result is always a cleaned path, the
// suffix is never left behind, and a path that did not ask for recursion never
// gets it. Anything else about the output is the caller's business.
func FuzzSplitRecursive(f *testing.F) {
	for _, seed := range []string{
		"", ".", "/", "...", "/...", "a/...", "/tmp/x/...", "/tmp/x",
		"//tmp//x//", "/tmp/.../x", "....", "/...//", "a/b/c/.../..",
		`C:\tmp\x`, `C:\tmp\x\...`, "\x00", "é/…/...", strings.Repeat("a/", 200) + "...",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, recursive := splitRecursive(in)

		if got != filepath.Clean(got) {
			t.Errorf("splitRecursive(%q) returned %q, which is not cleaned", in, got)
		}
		if got == "" {
			t.Errorf("splitRecursive(%q) returned an empty path", in)
		}

		// Exactly one marker is consumed, and what remains is a literal path.
		// "a/.../..." therefore means the directory "a/..." watched
		// recursively — "..." being a legal filename, the notation cannot
		// distinguish a marker from a directory genuinely named that, and
		// stripping repeatedly would silently widen the watch. What must not
		// happen is consuming none of it.
		if recursive && in == got {
			t.Errorf("splitRecursive(%q) reported recursion without consuming the marker", in)
		}

		// Recursion is opt-in: a path that does not end in the marker must
		// never be treated as recursive, or a caller would silently watch far
		// more than they asked for.
		if recursive {
			cleaned := filepath.Clean(in)
			if base := filepath.Base(cleaned); base != recursiveSuffix && cleaned != recursiveSuffix {
				t.Errorf("splitRecursive(%q) reported recursion for a path not ending in %q",
					in, recursiveSuffix)
			}
		}
	})
}

// FuzzExcluded checks the exclusion matcher against arbitrary patterns and
// paths.
//
// A malformed pattern must be inert rather than explosive: it is rejected at
// Add time, but the matcher itself is called from walks and from event
// delivery, where returning an error is not an option and panicking would take
// the program down.
func FuzzExcluded(f *testing.F) {
	for _, seed := range []struct{ pattern, root, path string }{
		{".git", "/src", "/src/.git"},
		{"*.tmp", "/src", "/src/x.tmp"},
		{"build/cache", "/src", "/src/build/cache"},
		{"[", "/src", "/src/x"},
		{"", "", ""},
		{"**", "/", "/a/b"},
		{"a\\", "/src", "/src/a"},
		{strings.Repeat("*", 100), "/src", "/src/" + strings.Repeat("a", 100)},
	} {
		f.Add(seed.pattern, seed.root, seed.path)
	}

	f.Fuzz(func(t *testing.T, pattern, root, path string) {
		o := defaultAddOpts()
		WithExclude(pattern).applyAdd(&o)

		// The only requirement is that it answers rather than panics; which
		// answer is right for an arbitrary pattern is not something a fuzzer
		// can decide.
		_ = o.excluded(root, path)

		// A pattern with no separator can never need the relative path, and
		// saying otherwise would reintroduce the cost that flag exists to
		// avoid.
		if !strings.ContainsRune(pattern, '/') && o.excludeHasPath {
			t.Errorf("pattern %q has no separator but was recorded as needing a relative path", pattern)
		}
	})
}

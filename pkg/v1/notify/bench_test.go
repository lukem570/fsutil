package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Adding and removing a watch is the operation a program doing anything
// dynamic performs most, and the one where per-call work is easiest to
// introduce without noticing.
func BenchmarkAddRemove(b *testing.B) {
	for _, kind := range Backends() {
		b.Run(kind.String(), func(b *testing.B) {
			w, err := NewWatcherWith(WithBackend(kind), WithEventBuffer(1024))
			if err != nil {
				b.Fatal(err)
			}
			defer w.Close()
			go drain(w)

			dir := b.TempDir()
			b.ReportAllocs()
			for b.Loop() {
				if err := w.Add(dir); err != nil {
					b.Fatal(err)
				}
				if err := w.Remove(dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Establishing a recursive watch walks the tree once. The cost is per
// directory, and this is what makes the difference between watching a
// repository feeling instant and feeling like a pause.
func BenchmarkAddRecursive(b *testing.B) {
	const dirs = 100

	for _, kind := range Backends() {
		b.Run(kind.String(), func(b *testing.B) {
			root := b.TempDir()
			for i := range dirs {
				if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%02d", i), "nested"), 0o755); err != nil {
					b.Fatal(err)
				}
			}

			w, err := NewWatcherWith(WithBackend(kind), WithEventBuffer(4096))
			if err != nil {
				b.Fatal(err)
			}
			defer w.Close()
			go drain(w)

			b.ReportAllocs()
			for b.Loop() {
				if err := w.AddWith(root, WithRecursive()); err != nil {
					b.Fatal(err)
				}
				if err := w.Remove(root); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Exclusion is consulted for every path considered during a walk and for every
// event delivered, so it sits on the hot path of anything watching a large
// tree. It must not be doing real work per call.
func BenchmarkExcludeMatching(b *testing.B) {
	o := defaultAddOpts()
	o.exclude = []string{".git", "node_modules", "*.tmp", "build/cache"}
	root := filepath.FromSlash("/src/project")
	path := filepath.FromSlash("/src/project/pkg/server/handler.go")

	b.ReportAllocs()
	for b.Loop() {
		if o.excluded(root, path) {
			b.Fatal("unexpected match")
		}
	}
}

// The same, with no patterns configured — the overwhelmingly common case,
// which must cost nothing at all.
func BenchmarkExcludeDisabled(b *testing.B) {
	o := defaultAddOpts()
	root := filepath.FromSlash("/src/project")
	path := filepath.FromSlash("/src/project/pkg/server/handler.go")

	b.ReportAllocs()
	for b.Loop() {
		if o.excluded(root, path) {
			b.Fatal("unexpected match")
		}
	}
}

func drain(w *Watcher) {
	for {
		select {
		case _, ok := <-w.Events:
			if !ok {
				return
			}
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}

package lock

import (
	"context"
	"path/filepath"
	"testing"
)

// An uncontended acquire and release should cost about two syscalls plus the
// process-local bookkeeping. If it ever costs meaningfully more, something has
// started doing work per acquisition that belongs at construction.
func BenchmarkAcquireRelease(b *testing.B) {
	l, err := New(filepath.Join(b.TempDir(), "bench.lock"))
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	b.ReportAllocs()
	for b.Loop() {
		if ok, err := l.TryLock(); err != nil || !ok {
			b.Fatalf("TryLock: (%v, %v)", ok, err)
		}
		if err := l.Unlock(); err != nil {
			b.Fatal(err)
		}
	}
}

// The blocking path over an uncontended lock, which should differ from the
// above only by the context plumbing — it must not consult its retry timer
// before trying once.
func BenchmarkLockUncontended(b *testing.B) {
	l, err := New(filepath.Join(b.TempDir(), "bench.lock"))
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if err := l.Lock(ctx); err != nil {
			b.Fatal(err)
		}
		if err := l.Unlock(); err != nil {
			b.Fatal(err)
		}
	}
}

// Goroutines contending for one lock never reach the kernel: the
// process-local registry resolves them first. This measures that path, which
// is the one a busy program actually spends time in.
func BenchmarkContendedInProcess(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.lock")

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		l, err := New(path)
		if err != nil {
			b.Error(err)
			return
		}
		defer l.Close()

		ctx := context.Background()
		for pb.Next() {
			if err := l.Lock(ctx); err != nil {
				b.Error(err)
				return
			}
			if err := l.Unlock(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

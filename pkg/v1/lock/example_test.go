package lock_test

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/lukem570/fsutil/pkg/v1/lock"
)

// Holding a lock across some work. Waiting honours the context, so a
// cancelled caller stops waiting rather than blocking in the kernel where
// nothing can interrupt it.
func Example() {
	l, err := lock.New(filepath.Join("testdata", "example.lock"), lock.WithCreateDirs())
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()

	if err := l.Lock(context.Background()); err != nil {
		log.Fatal(err)
	}
	defer l.Unlock()

	fmt.Println("holding:", l.Mode())
	// Output:
	// holding: exclusive
}

// Taking a lock only if it is free. A false return is an ordinary outcome —
// somebody else has it — rather than an error.
func ExampleLock_TryLock() {
	path := filepath.Join("testdata", "try.lock")

	first, err := lock.New(path, lock.WithCreateDirs())
	if err != nil {
		log.Fatal(err)
	}
	defer first.Close()

	second, err := lock.New(path)
	if err != nil {
		log.Fatal(err)
	}
	defer second.Close()

	got, err := first.TryLock()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("first:", got)

	got, err = second.TryLock()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("second:", got)

	_ = first.Unlock()
	// Output:
	// first: true
	// second: false
}

// Shared locks exclude writers but not each other.
func ExampleLock_RLock() {
	path := filepath.Join("testdata", "shared.lock")

	reader, err := lock.New(path, lock.WithCreateDirs())
	if err != nil {
		log.Fatal(err)
	}
	defer reader.Close()

	if err := reader.RLock(context.Background()); err != nil {
		log.Fatal(err)
	}
	defer reader.Unlock()

	fmt.Println("holding:", reader.Mode())
	// Output:
	// holding: shared
}

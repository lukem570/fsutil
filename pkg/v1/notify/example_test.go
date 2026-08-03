package notify_test

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/lukem570/fsutil/pkg/v1/notify"
)

// The common shape: one goroutine receiving, and work handed off rather than
// done in the receive loop. A consumer that blocks stalls the watcher, and can
// cause the kernel's own queue to overflow.
func Example() {
	w, err := notify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return // Close was called
				}
				if ev.Has(notify.Write) {
					log.Printf("modified: %s", ev.Name)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				// An error here does not mean watching has stopped.
				log.Printf("watch error: %s", err)
			}
		}
	}()

	if err := w.Add(os.TempDir()); err != nil {
		log.Fatal(err)
	}
}

// Watching a whole tree. The "/..." suffix and [notify.WithRecursive] mean the
// same thing.
func Example_recursive() {
	w, err := notify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	if err := w.Add(filepath.Join(os.TempDir(), "...")); err != nil {
		log.Fatal(err)
	}
}

// Skipping directories that would otherwise dominate a recursive watch.
//
// An excluded directory is pruned rather than filtered: no watch is placed on
// it and nothing beneath it is walked, so a repository's version-control
// directory costs nothing instead of thousands of kernel watches.
func Example_excludingDirectories() {
	w, err := notify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	err = w.AddWith(os.TempDir(),
		notify.WithRecursive(),
		notify.WithExclude(".git", "node_modules", "*.tmp"))
	if err != nil {
		log.Fatal(err)
	}
}

// Asking to hear only when a writer has finished, rather than on every write.
//
// Operations named Unportable are reported only where the host supports them.
// Requesting one from a backend that cannot produce it fails, rather than
// silently never firing.
func Example_waitingForACompletedWrite() {
	w, err := notify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	err = w.AddWith(os.TempDir(), notify.WithOps(notify.UnportableCloseWrite))
	if err != nil {
		// Not every platform can report this.
		log.Printf("close-write is unavailable here: %s", err)
	}
}

// Forcing the polling backend, which is the only one that works on network and
// virtual filesystems.
//
// Naming a backend the host cannot provide is an error rather than an
// invitation to substitute something else, which matters when the choice is
// load-bearing.
func Example_selectingABackend() {
	fmt.Println("available:", len(notify.Backends()) > 0)

	w, err := notify.NewWatcherWith(notify.WithBackend(notify.BackendPoll))
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	fmt.Println("chosen:", w.Backend())
	// Output:
	// available: true
	// chosen: poll
}

func ExampleOp_String() {
	fmt.Println(notify.Create)
	fmt.Println(notify.Create | notify.Write)
	// Output:
	// CREATE
	// CREATE|WRITE
}

func ExampleEvent_Has() {
	ev := notify.Event{Name: "/tmp/file", Op: notify.Create | notify.Write}

	fmt.Println(ev.Has(notify.Create))
	fmt.Println(ev.Has(notify.Remove))
	// Output:
	// true
	// false
}

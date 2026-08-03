// Command fsutil exercises this module's packages from a shell.
//
// It exists mainly so that a bug report can be reduced to a command. "Watching
// this directory does not report my editor's saves" is hard to act on; the
// output of `fsutil watch -debug ./dir` while reproducing it usually settles
// the question of whether the kernel reported anything at all.
//
// Usage:
//
//	fsutil watch [flags] <path>...      print filesystem events
//	fsutil lock [flags] <path> -- cmd   run a command holding a lock
//	fsutil backends                     list the backends this host offers
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/lukem570/fsutil/pkg/v1/lock"
	"github.com/lukem570/fsutil/pkg/v1/notify"
)

func main() { os.Exit(run()) }

// run holds the body of main so that deferred work still happens: os.Exit does
// not run deferred functions, so calling it from main would skip the signal
// handler's cleanup.
func run() int {
	if len(os.Args) < 2 {
		usage()
		return 2
	}

	// Interrupting is the normal way to stop `watch`, so it is a clean exit
	// rather than a signal death: the deferred Close has to run, or the
	// command would model exactly the mistake it exists to help diagnose.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "watch":
		err = watchCmd(ctx, os.Args[2:])
	case "lock":
		err = lockCmd(ctx, os.Args[2:])
	case "backends":
		err = backendsCmd()
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "fsutil: unknown command %q\n\n", os.Args[1])
		usage()
		return 2
	}

	var status *exitStatus
	if errors.As(err, &status) {
		return status.code
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "fsutil: %s\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `fsutil — filesystem watching and locking

  fsutil watch [flags] <path>...     print events as they happen
  fsutil lock [flags] <path> -- cmd  run a command while holding a lock
  fsutil backends                    list the backends available here

Run a subcommand with -h for its flags.
`)
}

func backendsCmd() error {
	available := notify.Backends()
	fmt.Printf("backends available on this host, best first:\n")
	for _, b := range available {
		fmt.Printf("  %s\n", b)
	}

	w, err := notify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	fmt.Printf("\nautomatic selection chose: %s\n", w.Backend())
	fmt.Printf("capabilities: %s\n", describeCaps(w.Capabilities()))
	fmt.Printf("\nlocking uses: %s (mandatory: %v)\n", lock.Mechanism(), lock.IsMandatory())
	return nil
}

func describeCaps(c notify.Capability) string {
	all := []struct {
		cap  notify.Capability
		name string
	}{
		{notify.CapRecursive, "recursive"},
		{notify.CapUnportableOps, "unportable-ops"},
		{notify.CapNoFollow, "no-follow"},
		{notify.CapPrivileged, "privileged"},
		{notify.CapFDPerPath, "fd-per-path"},
		{notify.CapFDPerDir, "fd-per-dir"},
		{notify.CapPreciseRename, "precise-rename"},
	}
	var have []string
	for _, x := range all {
		if c.Has(x.cap) {
			have = append(have, x.name)
		}
	}
	if len(have) == 0 {
		return "(none)"
	}
	return strings.Join(have, ", ")
}

func watchCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	recursive := fs.Bool("recursive", false, "watch each path and everything beneath it")
	backend := fs.String("backend", "", "force a backend (inotify, kqueue, poll, ...) instead of choosing one")
	interval := fs.Duration("interval", 0, "poll interval, for the polling backend")
	opsFlag := fs.String("ops", "", "comma-separated operations to report (create,write,remove,rename,chmod)")
	noFollow := fs.Bool("nofollow", false, "watch a symlink itself rather than its target")
	exclude := fs.String("exclude", "", "comma-separated patterns to skip, e.g. .git,node_modules,*.tmp")
	stats := fs.Duration("stats", 0, "report watch and descriptor counts on this interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("watch: no paths given")
	}

	var opts []notify.Option
	if *backend != "" {
		b, err := parseBackend(*backend)
		if err != nil {
			return err
		}
		opts = append(opts, notify.WithBackend(b))
	}
	if *interval > 0 {
		opts = append(opts, notify.WithPollInterval(*interval))
	}

	w, err := notify.NewWatcherWith(opts...)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	addOpts, err := buildAddOptions(*recursive, *noFollow, *opsFlag, *exclude)
	if err != nil {
		return err
	}
	for _, path := range fs.Args() {
		if err := w.AddWith(path, addOpts...); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "watching %d path(s) with the %s backend; interrupt to stop\n",
		len(w.WatchList()), w.Backend())

	// The documentation tells people to watch DescriptorsDenied to find out
	// whether a watcher has quietly stopped reporting modifications to some
	// files. That is only useful advice if there is a way to see it.
	var ticks <-chan time.Time
	if *stats > 0 {
		ticker := time.NewTicker(*stats)
		defer ticker.Stop()
		ticks = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			reportStats(w)
			return nil
		case <-ticks:
			reportStats(w)
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			fmt.Printf("%s %-13s %s\n", time.Now().Format("15:04:05.000"), ev.Op, ev.Name)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
		}
	}
}

// reportStats prints what the watcher is consuming, and says plainly when it
// has begun reporting less than it otherwise would.
func reportStats(w *notify.Watcher) {
	s := w.Stats()
	fmt.Fprintf(os.Stderr, "stats: %s\n", s)
	if s.DescriptorsDenied > 0 {
		fmt.Fprintf(os.Stderr,
			"  note: the descriptor budget was reached %d time(s), so modifications to some\n"+
				"  files are no longer reported. Creation, removal and renaming still are.\n"+
				"  Raise the process limit on open files, or watch fewer paths.\n",
			s.DescriptorsDenied)
	}
}

// buildAddOptions turns command-line flags into watch options.
//
// Assembling a list like this is exactly why AddOption is a named, exported
// type: without one, every caller whose options depend on configuration would
// be forced into a combinatorial switch.
func buildAddOptions(recursive, noFollow bool, ops, exclude string) ([]notify.AddOption, error) {
	var opts []notify.AddOption
	if recursive {
		opts = append(opts, notify.WithRecursive())
	}
	if noFollow {
		opts = append(opts, notify.WithNoFollow())
	}
	if exclude != "" {
		opts = append(opts, notify.WithExclude(strings.Split(exclude, ",")...))
	}
	if ops != "" {
		parsed, err := parseOps(ops)
		if err != nil {
			return nil, err
		}
		opts = append(opts, notify.WithOps(parsed...))
	}
	return opts, nil
}

func parseBackend(name string) (notify.Backend, error) {
	for _, b := range []notify.Backend{
		notify.BackendINotify, notify.BackendFANotify, notify.BackendKqueue,
		notify.BackendDirectoryChanges, notify.BackendUSNJournal,
		notify.BackendFEN, notify.BackendPoll,
	} {
		if b.String() == name {
			return b, nil
		}
	}
	return 0, fmt.Errorf("unknown backend %q; this host offers %v", name, notify.Backends())
}

func parseOps(list string) ([]notify.Op, error) {
	byName := map[string]notify.Op{
		"create":      notify.Create,
		"write":       notify.Write,
		"remove":      notify.Remove,
		"rename":      notify.Rename,
		"chmod":       notify.Chmod,
		"open":        notify.UnportableOpen,
		"read":        notify.UnportableRead,
		"close-write": notify.UnportableCloseWrite,
		"close-read":  notify.UnportableCloseRead,
	}

	var out []notify.Op
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		op, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown operation %q", name)
		}
		out = append(out, op)
	}
	return out, nil
}

func lockCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("lock", flag.ExitOnError)
	shared := fs.Bool("shared", false, "take a shared lock instead of an exclusive one")
	timeout := fs.Duration("timeout", 0, "give up waiting after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("lock: no lock file given")
	}
	path := rest[0]

	// Everything after "--" is the command to run while holding the lock.
	var command []string
	for i, arg := range rest {
		if arg == "--" {
			command = rest[i+1:]
			break
		}
	}
	if len(command) == 0 {
		return errors.New("lock: no command given; usage: fsutil lock <path> -- <command>")
	}

	var opts []lock.Option
	opts = append(opts, lock.WithCreateDirs(), lock.WithHolderInfo())
	if *timeout > 0 {
		opts = append(opts, lock.WithTimeout(*timeout))
	}

	l, err := lock.New(path, opts...)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	acquire := l.Lock
	if *shared {
		acquire = l.RLock
	}
	if err := acquire(ctx); err != nil {
		// Naming the holder turns "could not get the lock" into something
		// actionable, which is the whole reason the holder is recorded.
		if h, herr := l.Holder(); herr == nil && h.PID != 0 {
			return fmt.Errorf("%w (held by pid %d on %s since %s)",
				err, h.PID, h.Hostname, h.Since.Format(time.RFC3339))
		}
		return err
	}
	defer func() { _ = l.Unlock() }()

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Pass the command's own exit status through, so that this wrapper
			// is transparent to whatever called it.
			return &exitStatus{code: exitExitCode(exitErr)}
		}
		return err
	}
	return nil
}

// exitStatus carries a wrapped command's exit code back to run, so that this
// tool is transparent to whatever invoked it.
type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func exitExitCode(err *exec.ExitError) int { return err.ExitCode() }

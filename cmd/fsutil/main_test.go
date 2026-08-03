package main

import (
	"strings"
	"testing"

	"github.com/lukem570/fsutil/pkg/v1/notify"
)

// The command is mostly glue, and glue is where a typo survives review: a
// misspelled operation name or a backend the parser cannot find fails at the
// moment someone is already debugging something else.

func TestParseOps(t *testing.T) {
	tests := []struct {
		in   string
		want notify.Op
	}{
		{"create", notify.Create},
		{"create,write", notify.Create | notify.Write},
		{" create , remove ", notify.Create | notify.Remove},
		{"close-write", notify.UnportableCloseWrite},
		{"open,read,close-read", notify.UnportableOpen | notify.UnportableRead | notify.UnportableCloseRead},
	}
	for _, tt := range tests {
		got, err := parseOps(tt.in)
		if err != nil {
			t.Errorf("parseOps(%q): %s", tt.in, err)
			continue
		}
		var mask notify.Op
		for _, op := range got {
			mask |= op
		}
		if mask != tt.want {
			t.Errorf("parseOps(%q) = %s, want %s", tt.in, mask, tt.want)
		}
	}

	if _, err := parseOps("nonsense"); err == nil {
		t.Error("parseOps accepted an unknown operation")
	}
	// Every name the flag's help text advertises must actually parse, or the
	// documentation is a trap.
	for _, name := range []string{"create", "write", "remove", "rename", "chmod"} {
		if _, err := parseOps(name); err != nil {
			t.Errorf("parseOps(%q) failed but the flag help offers it: %s", name, err)
		}
	}
}

func TestParseBackend(t *testing.T) {
	for _, kind := range []notify.Backend{
		notify.BackendINotify, notify.BackendFANotify, notify.BackendKqueue,
		notify.BackendDirectoryChanges, notify.BackendUSNJournal,
		notify.BackendFEN, notify.BackendPoll,
	} {
		// Whatever a backend calls itself is what the flag must accept;
		// anything else means the two drifted apart.
		got, err := parseBackend(kind.String())
		if err != nil {
			t.Errorf("parseBackend(%q): %s", kind, err)
			continue
		}
		if got != kind {
			t.Errorf("parseBackend(%q) = %s, want %s", kind, got, kind)
		}
	}

	_, err := parseBackend("not-a-backend")
	if err == nil {
		t.Fatal("parseBackend accepted an unknown name")
	}
	// The error should say what is available, since the reader is choosing.
	if !strings.Contains(err.Error(), "poll") {
		t.Errorf("error does not mention what this host offers: %s", err)
	}
}

func TestBuildAddOptions(t *testing.T) {
	opts, err := buildAddOptions(true, true, "create,write", ".git,node_modules")
	if err != nil {
		t.Fatalf("buildAddOptions: %s", err)
	}
	if len(opts) != 4 {
		t.Errorf("got %d options, want 4 (recursive, nofollow, exclude, ops)", len(opts))
	}

	if opts, err := buildAddOptions(false, false, "", ""); err != nil || len(opts) != 0 {
		t.Errorf("buildAddOptions with no flags = (%d options, %v), want (0, nil)", len(opts), err)
	}
	if _, err := buildAddOptions(false, false, "bogus", ""); err == nil {
		t.Error("buildAddOptions accepted an unknown operation")
	}
}

func TestDescribeCaps(t *testing.T) {
	if got := describeCaps(0); got != "(none)" {
		t.Errorf("describeCaps(0) = %q, want %q", got, "(none)")
	}

	got := describeCaps(notify.CapRecursive | notify.CapPreciseRename)
	for _, want := range []string{"recursive", "precise-rename"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeCaps() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "privileged") {
		t.Errorf("describeCaps() = %q, naming a capability that was not set", got)
	}
}

func TestExitStatusCarriesTheCode(t *testing.T) {
	// A wrapped program's exit code has to survive being carried back as an
	// error, or `fsutil lock` stops being transparent to whatever called it.
	err := &exitStatus{code: 42}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("exitStatus.Error() = %q, does not mention the code", err.Error())
	}
}

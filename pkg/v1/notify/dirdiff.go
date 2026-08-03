package notify

import (
	"path/filepath"
	"sort"
)

// Shared by the backends whose kernel interface reports that a directory
// changed without saying how. kqueue and FEN both do this, and discovering
// which name appeared or vanished means comparing the listing against the
// previous one — a comparison that does not depend on which interface
// delivered the event.

// needsChildWatches reports whether the entries of a watched directory have to
// be opened individually.
//
// Only per-file events require it. A directory's own descriptor already
// reveals that an entry appeared or vanished, so a watch interested solely in
// creation, removal and renaming costs exactly one descriptor no matter how
// many files the directory holds. Since descriptors are the binding constraint
// on this platform, not opening them is worth the check.
func needsChildWatches(opts addOpts) bool {
	return opts.ops.Has(Write) || opts.ops.Has(Chmod)
}

// diffEntries turns two directory listings into events.
//
// A name that vanished and a name that appeared with the same file identity
// are two halves of one rename, so they are reported as such. Without identity
// they are reported as a removal and a creation, which is less precise but
// never wrong.
func diffEntries(dir string, prev, next map[string]fileID) []Event {
	var created, removed []string
	for name := range next {
		if _, ok := prev[name]; !ok {
			created = append(created, name)
		}
	}
	for name := range prev {
		if _, ok := next[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(created)
	sort.Strings(removed)

	movedFrom := make(map[string]bool)
	if fileIDSupported {
		byID := make(map[fileID]string, len(created))
		for _, name := range created {
			if id := next[name]; id != (fileID{}) {
				byID[id] = name
			}
		}
		for _, name := range removed {
			if id := prev[name]; id != (fileID{}) {
				if _, ok := byID[id]; ok {
					movedFrom[name] = true
				}
			}
		}
	}

	events := make([]Event, 0, len(created)+len(removed))
	for _, name := range removed {
		op := Remove
		if movedFrom[name] {
			op = Rename
		}
		events = append(events, Event{Name: filepath.Join(dir, name), Op: op})
	}
	for _, name := range created {
		events = append(events, Event{Name: filepath.Join(dir, name), Op: Create})
	}
	return events
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

package lock

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// holderRecordSize bounds what is read back from a lock file.
//
// The file is written by another process and may contain anything at all — a
// previous version of this program, an unrelated tool that chose the same
// path, or nothing but a stale fragment. Reading a bounded amount means a
// corrupt or enormous file costs a fixed amount of memory rather than however
// much someone else wrote.
const holderRecordSize = 4096

// writeHolder records this process's identity in the lock file.
//
// The file is truncated first: a previous holder's record may be longer than
// this one, and leaving its tail behind would produce a mixture of two records
// that parses as neither.
func writeHolder(f *os.File) error {
	hostname, _ := os.Hostname()
	record, err := json.Marshal(Holder{
		PID:      os.Getpid(),
		Hostname: hostname,
		Since:    time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	record = append(record, '\n')

	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(record, 0); err != nil {
		return err
	}
	// Not synced. The record is a diagnostic aid, and paying for a flush on
	// every acquisition to make a hint marginally more durable is a poor
	// trade; the lock itself does not depend on the file's contents.
	return nil
}

// readHolder reads back whatever identity the file records.
//
// An unparseable or empty file yields a zero Holder and no error. The file is
// not owned by this package in any meaningful sense — any process may write to
// it — so contents that make no sense are an expected condition rather than a
// failure.
func readHolder(f *os.File) (Holder, error) {
	buf := make([]byte, holderRecordSize)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return Holder{}, err
	}
	if n == 0 {
		return Holder{}, nil
	}

	var h Holder
	if err := json.Unmarshal(trimRecord(buf[:n]), &h); err != nil {
		// Deliberately not an error. Any process may write to this file, so
		// contents that make no sense are expected rather than exceptional,
		// and the caller wants "nothing recorded" rather than a failure.
		return Holder{}, nil //nolint:nilerr // see above
	}
	return h, nil
}

// trimRecord cuts the buffer at the end of the first line, since the rest is
// padding or another process's leftovers.
func trimRecord(b []byte) []byte {
	for i, c := range b {
		if c == '\n' || c == 0 {
			return b[:i]
		}
	}
	return b
}

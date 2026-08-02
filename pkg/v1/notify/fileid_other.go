//go:build !unix

package notify

import "io/fs"

// fileIDSupported reports whether this platform can identify a file
// independently of its name.
//
// It is false here. Windows can supply an equivalent identity through
// GetFileInformationByHandle, but that requires opening a handle to every
// file, which is precisely the per-path descriptor cost the polling backend
// exists to avoid. Plan 9 and WebAssembly expose nothing suitable.
//
// The consequence is confined to rename detection: without stable identity a
// move is reported as a removal followed by a creation. That is less precise
// than a rename, but it is not wrong, and every other operation is unaffected.
const fileIDSupported = false

// fileID identifies a file independently of its name. It carries no data on
// platforms that cannot supply one, and exists so that callers need no build
// tags of their own.
type fileID struct{}

// statID reports false on platforms without a usable file identity.
func statID(fs.FileInfo) (fileID, bool) { return fileID{}, false }

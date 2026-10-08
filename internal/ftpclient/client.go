// Package ftpclient is the seam between the binding and the FTP library.
//
// Only jlaffaye.go imports github.com/jlaffaye/ftp. Everything else in this
// module depends on the interfaces here, so the library (which is v0.x) can be
// replaced without touching the binding, and so the binding is testable
// without a socket.
package ftpclient

import (
	"context"
	"io"
	"time"
)

// EntryType classifies a directory listing entry.
type EntryType uint8

// The entry kinds a listing can report. Anything the server describes as
// neither a directory nor a symlink is treated as a regular file.
const (
	EntryFile EntryType = iota
	EntryDir
	EntryLink
)

func (t EntryType) String() string {
	switch t {
	case EntryDir:
		return "dir"
	case EntryLink:
		return "link"
	default:
		return "file"
	}
}

// Entry is one item from a directory listing.
type Entry struct {
	Name    string
	Type    EntryType
	Size    uint64
	ModTime time.Time
}

// Conn is a single authenticated FTP session.
//
// A Conn is NOT safe for concurrent use: the underlying protocol multiplexes a
// control channel and at most one data connection, so exactly one method may
// be in flight at a time. The pool is what enforces this.
type Conn interface {
	// Store uploads r to remotePath, replacing any existing file.
	Store(ctx context.Context, remotePath string, r io.Reader) error

	// Retrieve downloads remotePath into w and reports how many bytes were
	// written. Implementations must propagate the server's post-transfer
	// status, which FTP reports only after the data stream ends.
	Retrieve(ctx context.Context, remotePath string, w io.Writer) (int64, error)

	// Delete removes a single file. It is not expected to remove directories.
	Delete(ctx context.Context, remotePath string) error

	// List returns the entries of a directory. Listing a file is permitted and
	// yields a single entry on most servers.
	List(ctx context.Context, remotePath string) ([]Entry, error)

	// Broken reports that the session's protocol state is no longer
	// trustworthy, typically after an aborted transfer. A broken Conn must be
	// closed rather than reused: the control stream may be mid-reply, and
	// reusing it produces cross-talk between unrelated requests that is
	// miserable to debug.
	//
	// There is no liveness probe. A session that died while idle surfaces as a
	// connection-level failure on its next use, which the binding retries once
	// on a fresh session — the same approach the built-in SFTP binding takes.
	Broken() bool

	// Close sends QUIT on a best-effort basis and releases the sockets.
	// It is safe to call more than once.
	Close() error
}

// Dialer opens authenticated sessions.
type Dialer interface {
	// Dial connects and logs in. The context bounds connection establishment
	// and login only; it does not bound the returned Conn's lifetime.
	Dial(ctx context.Context) (Conn, error)

	// Target is a credential-free description used in error messages.
	Target() string
}

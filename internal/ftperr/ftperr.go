// Package ftperr provides the error type the binding surfaces to Dapr.
//
// An error returned from OutputBinding.Invoke reaches the operator as a single
// flat string in an HTTP 500 body or a gRPC status: no structured fields, no
// error codes of our own, no stack. That one sentence is the entire diagnostic
// budget, so it must carry the operation, the resolved server path, a
// credential-free target, and the server's own reply verbatim.
package ftperr

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"syscall"
)

// ErrBrokenConn marks a session whose protocol state is no longer trustworthy.
var ErrBrokenConn = errors.New("ftp: connection is no longer usable")

// Error carries everything an operator needs to triage a failed operation.
type Error struct {
	Op     string // "create", "get", "list", "delete", "connect"
	Path   string // resolved server path, or empty
	Target string // e.g. "ftps-explicit://ftp.example.com:21" - never credentials
	Code   int    // FTP reply code, 0 when the failure was not a server reply
	Err    error
}

func (e *Error) Error() string {
	msg := "ftp: " + e.Op
	if e.Path != "" {
		msg += fmt.Sprintf(" %q", e.Path)
	}
	if e.Target != "" {
		msg += " on " + e.Target
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Wrap annotates err unless it is nil.
func Wrap(op, path, target string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, Path: path, Target: target, Code: Code(err), Err: err}
}

// Code returns the FTP reply code carried by err, or 0.
func Code(err error) int {
	var pe *textproto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return 0
}

// FTP reply codes we classify. See RFC 959 section 4.2.
const (
	CodeDataConnOpen      = 425 // can't open data connection
	CodeTransferAborted   = 426 // connection closed, transfer aborted
	CodeServiceClosing    = 421 // service not available, closing control connection
	CodeNotLoggedIn       = 530 // not logged in
	CodeFileUnavailable   = 550 // file unavailable: not found, or no permission
	CodeNameNotAllowed    = 553 // file name not allowed
	CodeStorageExceeded   = 552 // exceeded storage allocation
	CodeNeedAccount       = 532 // need account for storing files
	CodeFileBusy          = 450 // file unavailable, busy
	CodeInsufficientSpace = 452 // insufficient storage space
)

// IsConnectionLevel reports whether err means the session itself is unusable,
// as opposed to the server refusing a specific command. Only connection-level
// failures are worth retrying on a fresh connection.
func IsConnectionLevel(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBrokenConn) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	switch Code(err) {
	case CodeServiceClosing, CodeDataConnOpen, CodeTransferAborted, CodeNotLoggedIn:
		return true
	}
	return false
}

// IsNotFound reports whether err is the server saying the file is unavailable.
//
// FTP cannot distinguish "no such file" from "permission denied": both are 550.
// Callers must not present this as definitively "not found".
func IsNotFound(err error) bool { return Code(err) == CodeFileUnavailable }

// Explain returns a short clarification for reply codes whose standard text is
// unhelpful, or "" when the server's own message suffices.
func Explain(code int) string {
	switch code {
	case CodeFileUnavailable:
		return "the file does not exist, or the account lacks permission (FTP reports both as 550)"
	case CodeNameNotAllowed:
		return "the server rejected the file name"
	case CodeStorageExceeded:
		return "the account is over its storage quota"
	case CodeNeedAccount:
		return "the server requires an ACCT command, which this binding does not support"
	case CodeFileBusy, CodeInsufficientSpace:
		return "the server reported a temporary failure; retrying later may succeed"
	case CodeNotLoggedIn:
		return "the session lost its authentication"
	case CodeServiceClosing:
		return "the server closed the control connection"
	}
	return ""
}

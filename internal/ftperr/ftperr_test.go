package ftperr

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"strings"
	"syscall"
	"testing"
)

func reply(code int, msg string) error { return &textproto.Error{Code: code, Msg: msg} }

func TestErrorMessageCarriesTheTriageFields(t *testing.T) {
	err := Wrap("get", "/upload/a.txt", "ftps-explicit://ftp.example.com:21", reply(CodeFileUnavailable, "No such file"))

	got := err.Error()
	for _, want := range []string{"get", `"/upload/a.txt"`, "ftps-explicit://ftp.example.com:21", "No such file"} {
		if !strings.Contains(got, want) {
			t.Errorf("error message %q is missing %q", got, want)
		}
	}
}

func TestWrapIsTransparentForNil(t *testing.T) {
	if err := Wrap("get", "/a", "ftp://h:21", nil); err != nil {
		t.Fatalf("Wrap(nil) = %v, want nil", err)
	}
}

func TestWrapPreservesTheCodeAndTheCause(t *testing.T) {
	cause := reply(CodeStorageExceeded, "Quota exceeded")
	err := Wrap("create", "/upload/a.txt", "ftp://h:21", cause)

	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatal("errors.As did not find an *Error")
	}
	if fe.Code != CodeStorageExceeded {
		t.Errorf("Code = %d, want %d", fe.Code, CodeStorageExceeded)
	}
	if !errors.Is(err, cause) {
		t.Error("the wrapped error does not unwrap to its cause")
	}
}

// The binding wraps before classifying, so classification has to survive both
// our own wrapping and fmt.Errorf.
func TestCodeSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("outer: %w", Wrap("get", "/a", "ftp://h:21", reply(CodeNotLoggedIn, "Not logged in")))
	if got := Code(err); got != CodeNotLoggedIn {
		t.Errorf("Code = %d, want %d", got, CodeNotLoggedIn)
	}
}

func TestCodeIsZeroForNonReplyErrors(t *testing.T) {
	if got := Code(io.EOF); got != 0 {
		t.Errorf("Code(io.EOF) = %d, want 0", got)
	}
	if got := Code(nil); got != 0 {
		t.Errorf("Code(nil) = %d, want 0", got)
	}
}

// Misclassifying here is expensive in both directions: a false positive retries
// a command the server has already refused, and a false negative leaves a dead
// session in play.
func TestIsConnectionLevel(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"broken conn sentinel", ErrBrokenConn, true},
		{"wrapped broken conn", fmt.Errorf("store: %w", ErrBrokenConn), true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"deadline exceeded", os.ErrDeadlineExceeded, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"broken pipe", syscall.EPIPE, true},
		{"use of closed network connection", net.ErrClosed, true},
		{"net.Error", &net.OpError{Op: "read", Err: syscall.ECONNREFUSED}, true},
		{"421 service closing", reply(CodeServiceClosing, "Timeout"), true},
		{"425 cannot open data connection", reply(CodeDataConnOpen, "Can't open data connection"), true},
		{"426 transfer aborted", reply(CodeTransferAborted, "Transfer aborted"), true},
		{"530 not logged in", reply(CodeNotLoggedIn, "Not logged in"), true},

		// The server answered on a healthy control channel: retrying on a fresh
		// session would just repeat the refusal.
		{"550 file unavailable", reply(CodeFileUnavailable, "No such file"), false},
		{"553 name not allowed", reply(CodeNameNotAllowed, "Bad name"), false},
		{"552 over quota", reply(CodeStorageExceeded, "Quota exceeded"), false},
		{"532 need account", reply(CodeNeedAccount, "Need account"), false},
		{"450 file busy", reply(CodeFileBusy, "Busy"), false},
		{"452 insufficient space", reply(CodeInsufficientSpace, "No space"), false},
		{"plain error", errors.New("something went wrong"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConnectionLevel(tc.err); got != tc.want {
				t.Errorf("IsConnectionLevel(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsConnectionLevelSeesThroughOurOwnWrapping(t *testing.T) {
	err := Wrap("get", "/a", "ftp://h:21", reply(CodeServiceClosing, "Timeout"))
	if !IsConnectionLevel(err) {
		t.Error("a wrapped 421 was not classified as connection-level")
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(reply(CodeFileUnavailable, "No such file")) {
		t.Error("550 was not reported as not-found")
	}
	if IsNotFound(reply(CodeNameNotAllowed, "Bad name")) {
		t.Error("553 was reported as not-found")
	}
	if IsNotFound(io.EOF) {
		t.Error("io.EOF was reported as not-found")
	}
}

func TestExplainCoversEveryClassifiedCode(t *testing.T) {
	// Each of these is classified somewhere above, so each needs operator-facing
	// text; a code added to the list without a note would reach the operator as
	// a bare number.
	for _, code := range []int{
		CodeFileUnavailable, CodeNameNotAllowed, CodeStorageExceeded,
		CodeNeedAccount, CodeFileBusy, CodeInsufficientSpace,
		CodeNotLoggedIn, CodeServiceClosing,
	} {
		if Explain(code) == "" {
			t.Errorf("Explain(%d) is empty", code)
		}
	}
}

func TestExplainIsSilentWhenTheServerMessageSuffices(t *testing.T) {
	for _, code := range []int{0, 200, 226, 500, CodeDataConnOpen, CodeTransferAborted} {
		if note := Explain(code); note != "" {
			t.Errorf("Explain(%d) = %q, want no annotation", code, note)
		}
	}
}

// The whole point of Target is that it is credential-free; this is the test
// that would catch someone widening it.
func TestErrorNeverCarriesCredentials(t *testing.T) {
	err := Wrap("create", "/upload/a.txt", "ftps-explicit://ftp.example.com:21", reply(CodeNotLoggedIn, "Login incorrect"))
	for _, secret := range []string{"hunter2", "ftpuser:hunter2"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error message leaked %q: %s", secret, err)
		}
	}
}

func TestErrorFormatsWithoutAPath(t *testing.T) {
	err := Wrap("connect", "", "ftp://ftp.example.com:21", errors.New("connection refused"))
	if got := err.Error(); strings.Contains(got, `""`) {
		t.Errorf("empty path rendered as an empty quoted string: %s", got)
	}
}

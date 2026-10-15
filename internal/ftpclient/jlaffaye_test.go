package ftpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
)

func dialerFor(ts *testServer, mode TLSMode, tlsCfg *tls.Config) Dialer {
	return NewDialer(DialerConfig{
		Address:    ts.addr,
		Username:   testUser,
		Password:   testPass,
		TLSMode:    mode,
		TLSConfig:  tlsCfg,
		Timeout:    10 * time.Second,
		TargetName: "test://" + ts.addr,
	})
}

func connect(t *testing.T, ts *testServer, mode TLSMode, tlsCfg *tls.Config) Conn {
	t.Helper()
	c, err := dialerFor(ts, mode, tlsCfg).Dial(context.Background())
	if err != nil {
		t.Fatalf("dial (%v): %v", mode, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Binary content with CRLF and NUL bytes: if the adapter forgot TYPE I the
// server would mangle these in ASCII mode and the round trip would differ.
var binaryPayload = []byte("line1\r\nline2\n\x00\x01\x02\xff binary\r\n")

func TestRoundTripPlain(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)
	ctx := context.Background()

	if err := c.Store(ctx, "/upload/bin.dat", bytes.NewReader(binaryPayload)); err != nil {
		t.Fatalf("store: %v", err)
	}
	var buf bytes.Buffer
	n, err := c.Retrieve(ctx, "/upload/bin.dat", &buf)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if int(n) != len(binaryPayload) || !bytes.Equal(buf.Bytes(), binaryPayload) {
		t.Fatalf("round trip corrupted the payload:\n got %q\nwant %q", buf.Bytes(), binaryPayload)
	}

	entries, err := c.List(ctx, "/upload")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasEntry(entries, "bin.dat") {
		t.Errorf("list = %+v, want bin.dat", entries)
	}

	if err := c.Delete(ctx, "/upload/bin.dat"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Retrieve(ctx, "/upload/bin.dat", &bytes.Buffer{}); err == nil {
		t.Error("retrieve after delete should fail")
	}
}

// What this actually covers is our TLSMode -> dial-option mapping: explicit must
// pick DialWithExplicitTLS and implicit DialWithTLS, which are easy to swap.
//
// It does NOT prove the data channel is encrypted. The test server accepts a
// cleartext data connection, so a regression that dropped data-channel TLS would
// still pass here. That assertion lives in the integration suite, where the
// ProFTPD containers run "TLSRequired on" and refuse cleartext data.
func TestTLSRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     TLSMode
		required ftpserver.TLSRequirement
	}{
		{"explicit", TLSExplicit, ftpserver.ClearOrEncrypted},
		{"implicit", TLSImplicit, ftpserver.ImplicitEncryption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startServer(t, serverOpts{withTLS: true, tlsRequired: tc.required})
			c := connect(t, ts, tc.mode, ts.clientTLS(t))
			ctx := context.Background()

			if err := c.Store(ctx, "/upload/tls.dat", bytes.NewReader(binaryPayload)); err != nil {
				t.Fatalf("store over TLS: %v", err)
			}
			var buf bytes.Buffer
			if _, err := c.Retrieve(ctx, "/upload/tls.dat", &buf); err != nil {
				t.Fatalf("retrieve over TLS: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), binaryPayload) {
				t.Fatal("TLS round trip corrupted the payload")
			}
		})
	}
}

func TestRetrieveMissingFileReports550(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)

	_, err := c.Retrieve(context.Background(), "/upload/nope.dat", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if c.Broken() {
		t.Error("a command-level refusal must not poison the session")
	}
}

func TestDialFailsOnBadCredentials(t *testing.T) {
	ts := startServer(t, serverOpts{})
	d := NewDialer(DialerConfig{
		Address: ts.addr, Username: "wrong", Password: "wrong",
		Timeout: 5 * time.Second, TargetName: "test",
	})
	_, err := d.Dial(context.Background())
	if err == nil {
		t.Fatal("expected authentication to fail")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error should mention login, got %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c, err := dialerFor(ts, TLSNone, nil).Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if err := c.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

func hasEntry(entries []Entry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

// We no longer screen file names ourselves; the library does it on every
// command path (ftp.go:626 checkForCommandInjection, reached from Stor, Retr,
// List and Delete). This fails loudly if a version bump regresses that, since
// CR/LF in a path is the command-injection vector of GHSA-3wqq-h39g-266f.
func TestLibraryRejectsControlCharactersInPaths(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)
	ctx := context.Background()

	const injected = "a.txt\r\nDELE /upload/victim.dat"

	if err := c.Store(ctx, "/upload/victim.dat", bytes.NewReader([]byte("keep me"))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for name, op := range map[string]func() error{
		"store":    func() error { return c.Store(ctx, injected, bytes.NewReader(nil)) },
		"retrieve": func() error { _, err := c.Retrieve(ctx, injected, &bytes.Buffer{}); return err },
		"delete":   func() error { return c.Delete(ctx, injected) },
		"list":     func() error { _, err := c.List(ctx, injected); return err },
	} {
		if err := op(); err == nil {
			t.Errorf("%s accepted a path containing CR/LF", name)
		}
	}

	// The smuggled DELE must not have run.
	var buf bytes.Buffer
	if _, err := c.Retrieve(ctx, "/upload/victim.dat", &buf); err != nil {
		t.Fatalf("the injected DELE appears to have executed: %v", err)
	}
	if buf.String() != "keep me" {
		t.Errorf("victim file = %q, want %q", buf.String(), "keep me")
	}
}

// FTP reports a transfer's outcome on the control channel, after the data
// connection has already closed. A server-side failure therefore looks like a
// clean, short read to the client, and is visible only in Response.Close().
// Discarding that status makes a failed download look successful with partial
// data — the defect upstream PR dapr/components-contrib#4593 shipped.
//
// This exercises the real adapter. The equivalent binding-level test drives a
// hand-written fake, so it proves only that the binding propagates an error it
// is handed; it cannot prove the adapter captures one.
func TestRetrievePropagatesPostTransferFailure(t *testing.T) {
	const (
		total  = 64 << 10
		served = 1 << 10
	)
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)
	ctx := context.Background()

	if err := c.Store(ctx, "/upload/truncated.dat", bytes.NewReader(bytes.Repeat([]byte("x"), total))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// From now on the server serves 1 KiB of any download and then fails.
	ts.driver.failReadAfter = served

	var buf bytes.Buffer
	n, err := c.Retrieve(ctx, "/upload/truncated.dat", &buf)
	if err == nil {
		t.Fatalf("retrieve reported success after a server-side failure, returning %d of %d bytes", n, total)
	}
	if n >= total {
		t.Fatalf("expected a short read, got %d of %d bytes", n, total)
	}
	t.Logf("correctly failed after %d of %d bytes: %v", n, total, err)
}

// Rename over a real socket. This is the only tier that proves the library
// really does RNFR/RNTO on the control channel and that a server accepts it;
// the fake-driven tests above the seam cannot tell.
func TestRenameMovesWithoutReTransferring(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)
	ctx := context.Background()

	if err := c.Store(ctx, "/upload/a.dat.tmp", bytes.NewReader(binaryPayload)); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := c.Rename(ctx, "/upload/a.dat.tmp", "/upload/a.dat"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	entries, err := c.List(ctx, "/upload")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if hasEntry(entries, "a.dat.tmp") {
		t.Error("the temp name is still listed after the rename")
	}
	if !hasEntry(entries, "a.dat") {
		t.Fatalf("list = %+v, want a.dat", entries)
	}

	// Content must be byte-identical: a rename moves the directory entry, it
	// does not re-transfer, so TYPE I is irrelevant and nothing can mangle it.
	var buf bytes.Buffer
	if _, err := c.Retrieve(ctx, "/upload/a.dat", &buf); err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), binaryPayload) {
		t.Errorf("content changed across the rename:\n got %q\nwant %q", buf.Bytes(), binaryPayload)
	}
}

// A rename whose source does not exist is a command-level refusal, so the
// session stays usable -- withConn must report it rather than retry.
func TestRenameMissingSourceReports550(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)

	err := c.Rename(context.Background(), "/upload/nope.tmp", "/upload/nope")
	if err == nil {
		t.Fatal("expected an error renaming a file that does not exist")
	}
	if c.Broken() {
		t.Error("a 550 is the server's answer, not a transport failure; the session is still usable")
	}
}

// MKD is wired through and creates one level. The fake-driven tests above the
// seam cannot cover this: only a real server decides what MKD means.
func TestMakeDirCreatesOneLevel(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)
	ctx := context.Background()

	if err := c.MakeDir(ctx, "/upload/sub"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entries, err := c.List(ctx, "/upload")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasEntry(entries, "sub") {
		t.Fatalf("list = %+v, want sub", entries)
	}
	if err := c.Store(ctx, "/upload/sub/a.dat", bytes.NewReader(binaryPayload)); err != nil {
		t.Fatalf("store into the new directory: %v", err)
	}
}

// Creating a directory that is already there is a refusal, not a no-op. The
// binding depends on that being command-level: it walks a whole chain and
// expects the levels that already exist to fail harmlessly.
func TestMakeDirOnAnExistingDirectoryIsCommandLevel(t *testing.T) {
	ts := startServer(t, serverOpts{})
	c := connect(t, ts, TLSNone, nil)
	ctx := context.Background()

	if err := c.MakeDir(ctx, "/upload/sub"); err != nil {
		t.Fatalf("first mkdir: %v", err)
	}
	if err := c.MakeDir(ctx, "/upload/sub"); err == nil {
		t.Error("expected an error creating a directory that already exists")
	}
	if c.Broken() {
		t.Fatal("a refused MKD must leave the session usable")
	}
	// Still usable, which is what lets the binding keep walking the chain.
	if err := c.Store(ctx, "/upload/sub/a.dat", bytes.NewReader(binaryPayload)); err != nil {
		t.Fatalf("store after the refused mkdir: %v", err)
	}
}

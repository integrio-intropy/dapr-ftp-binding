package binding

import (
	"context"
	"encoding/json"
	"io"
	"net/textproto"
	"strings"
	"testing"

	"github.com/dapr/components-contrib/bindings"
	"github.com/dapr/kit/logger"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/config"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient/fake"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftperr"
)

func newBinding(t *testing.T, b *fake.Backend, extra map[string]string) *Binding {
	t.Helper()
	props := map[string]string{
		"address":  "ftp.example.com:21",
		"username": "user",
		"password": "pass",
		"rootPath": "/upload",
	}
	for k, v := range extra {
		props[k] = v
	}

	bind := New(logger.NewLogger("test"), func(*config.Config) (ftpclient.Dialer, error) {
		return b.Dialer(), nil
	})
	if err := bind.Init(context.Background(), bindings.Metadata{
		Base: metadataBase(props),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = bind.Close() })
	return bind
}

func invoke(t *testing.T, b *Binding, op bindings.OperationKind, data []byte, md map[string]string) (*bindings.InvokeResponse, error) {
	t.Helper()
	return b.Invoke(context.Background(), &bindings.InvokeRequest{
		Operation: op, Data: data, Metadata: md,
	})
}

func TestOperationsExcludesRename(t *testing.T) {
	b := newBinding(t, fake.NewBackend(), nil)
	ops := b.Operations()
	if len(ops) != 4 {
		t.Fatalf("Operations() = %v, want 4", ops)
	}
	for _, op := range ops {
		if string(op) == "rename" {
			t.Error("rename is deferred and must not be advertised")
		}
	}
}

func TestCreateRoundTrip(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, nil)

	resp, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var cr createResponse
	if err := json.Unmarshal(resp.Data, &cr); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if cr.FileName != "/upload/a.txt" {
		t.Errorf("fileName = %q, want /upload/a.txt", cr.FileName)
	}
	if resp.Metadata["fileName"] != "/upload/a.txt" {
		t.Errorf("response metadata fileName = %q", resp.Metadata["fileName"])
	}
	if resp.ContentType == nil || *resp.ContentType != contentTypeJSON {
		t.Errorf("ContentType = %v, want %s", resp.ContentType, contentTypeJSON)
	}
	if got, ok := backend.Get("/upload/a.txt"); !ok || string(got) != "hello" {
		t.Errorf("stored = %q (ok=%v), want hello", got, ok)
	}
}

func TestGetReturnsRawBytes(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/upload/a.txt", []byte("contents"))
	b := newBinding(t, backend, nil)

	resp, err := invoke(t, b, bindings.GetOperation, nil, map[string]string{"fileName": "a.txt"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(resp.Data) != "contents" {
		t.Errorf("Data = %q, want contents", resp.Data)
	}
	if resp.Metadata["size"] != "8" {
		t.Errorf("size metadata = %q, want 8", resp.Metadata["size"])
	}
	if resp.ContentType == nil || *resp.ContentType != contentTypeBinary {
		t.Errorf("ContentType = %v", resp.ContentType)
	}
}

// The listing shape matches the built-in SFTP binding's.
func TestListReturnsEntries(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/upload/a.txt", []byte("a"))
	backend.Put("/upload/sub/b.txt", []byte("b"))
	b := newBinding(t, backend, nil)

	resp, err := invoke(t, b, bindings.ListOperation, nil, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var entries []listEntry
	if err := json.Unmarshal(resp.Data, &entries); err != nil {
		t.Fatalf("response is not a JSON array: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2", entries)
	}

	byName := map[string]listEntry{}
	for _, e := range entries {
		byName[e.FileName] = e
	}
	if e, ok := byName["a.txt"]; !ok || e.IsDirectory {
		t.Errorf("a.txt = %+v, want a file", e)
	}
	if e, ok := byName["sub"]; !ok || !e.IsDirectory {
		t.Errorf("sub = %+v, want a directory", e)
	}
	if resp.ContentType == nil || *resp.ContentType != contentTypeJSON {
		t.Errorf("ContentType = %v", resp.ContentType)
	}
}

// Paths are joined, not confined: like the SFTP binding, the server decides
// what the account can reach. This records that as the intended behaviour.
func TestFileNameIsJoinedNotConfined(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/secret", []byte("classified"))
	b := newBinding(t, backend, nil)

	resp, err := invoke(t, b, bindings.GetOperation, nil, map[string]string{"fileName": "../secret"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(resp.Data) != "classified" {
		t.Errorf("Data = %q; path.Join should have resolved ../secret to /secret", resp.Data)
	}
}

func TestDeleteReturnsEmptyResponse(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/upload/a.txt", []byte("x"))
	b := newBinding(t, backend, nil)

	resp, err := invoke(t, b, bindings.DeleteOperation, nil, map[string]string{"fileName": "a.txt"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Non-nil but empty: the sidecar turns this into a 204, and returning nil
	// risks a nil dereference in the SDK's marshalling.
	if resp == nil {
		t.Fatal("delete returned a nil response")
	}
	if len(resp.Data) != 0 {
		t.Errorf("Data = %q, want empty", resp.Data)
	}
	if _, ok := backend.Get("/upload/a.txt"); ok {
		t.Error("file was not deleted")
	}
}

func TestCreateRequiresFileName(t *testing.T) {
	b := newBinding(t, fake.NewBackend(), nil)
	_, err := invoke(t, b, bindings.CreateOperation, []byte("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "fileName is missing") {
		t.Fatalf("error = %v, want a missing fileName error", err)
	}
}

func TestDeleteCannotTargetRoot(t *testing.T) {
	b := newBinding(t, fake.NewBackend(), nil)
	_, err := invoke(t, b, bindings.DeleteOperation, nil, map[string]string{"fileName": ""})
	if err == nil {
		t.Fatal("delete with no fileName must not target rootPath")
	}
}

// The upstream bug: a server-side failure reported only after the data stream
// ends must not be swallowed.
func TestTransferStatusAfterStreamIsNotSwallowed(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/upload/a.txt", []byte("partial"))
	backend.FailRetrieveClose = true
	b := newBinding(t, backend, nil)

	resp, err := invoke(t, b, bindings.GetOperation, nil, map[string]string{"fileName": "a.txt"})
	if err == nil {
		t.Fatalf("a post-transfer failure was reported as success with data %q", resp.Data)
	}
}

// Connection-level failures retry exactly once, on a fresh session.
func TestConnectionLevelErrorRetriesOnce(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/upload/a.txt", []byte("ok"))
	b := newBinding(t, backend, nil)

	before := backend.Dials()
	var failed bool
	backend.FailNext = func(op, _ string) error {
		if op == "retrieve" && !failed {
			failed = true
			return io.ErrUnexpectedEOF
		}
		return nil
	}

	resp, err := invoke(t, b, bindings.GetOperation, nil, map[string]string{"fileName": "a.txt"})
	if err != nil {
		t.Fatalf("get should have succeeded on the retry: %v", err)
	}
	if string(resp.Data) != "ok" {
		t.Errorf("Data = %q", resp.Data)
	}
	if backend.Dials() <= before {
		t.Error("the retry should have dialled a fresh connection")
	}
}

// A command-level refusal (550) is the server's answer, not a transport
// problem, so it must not be retried.
func TestCommandLevelErrorIsNotRetried(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, nil)

	var attempts int
	backend.FailNext = func(op, _ string) error {
		if op == "delete" {
			attempts++
			return &textproto.Error{Code: ftperr.CodeFileUnavailable, Msg: "Permission denied"}
		}
		return nil
	}

	_, err := invoke(t, b, bindings.DeleteOperation, nil, map[string]string{"fileName": "a.txt"})
	if err == nil {
		t.Fatal("expected a 550")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a 550 must not be retried)", attempts)
	}
	// The operator needs the resolved path and the server's own text.
	for _, want := range []string{"/upload/a.txt", "Permission denied", "550"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

func TestErrorsNeverLeakCredentials(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, nil)
	backend.FailNext = func(_, _ string) error {
		return &textproto.Error{Code: 550, Msg: "nope"}
	}
	_, err := invoke(t, b, bindings.GetOperation, nil, map[string]string{"fileName": "a.txt"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "pass") {
		t.Fatalf("error leaked the password: %v", err)
	}
}

func TestUnknownOperation(t *testing.T) {
	b := newBinding(t, fake.NewBackend(), nil)
	_, err := invoke(t, b, bindings.OperationKind("rename"), nil, map[string]string{"fileName": "a"})
	if err == nil || !strings.Contains(err.Error(), "unsupported operation") {
		t.Fatalf("error = %v", err)
	}
}

// The probe connection must not end up in the pool.
func TestInitDoesNotPoolTheProbeConnection(t *testing.T) {
	backend := fake.NewBackend()
	newBinding(t, backend, nil)
	if backend.LiveSessions() != 0 {
		t.Errorf("Init left %d sessions open; the probe should be closed", backend.LiveSessions())
	}
}

func TestGetComponentMetadataListsFields(t *testing.T) {
	b := newBinding(t, fake.NewBackend(), nil)
	m := b.GetComponentMetadata()
	if len(m) == 0 {
		t.Fatal("GetComponentMetadata returned nothing")
	}
}

// Init is a gRPC method the sidecar can call more than once (restart, hot
// reload), and main.go caches one *Binding per instance id for the life of the
// process. Replacing the pool without closing the old one strands its sessions
// logged in and its reaper goroutine running, and Close can then only reach the
// newest pool.
func TestReinitClosesPreviousPool(t *testing.T) {
	backend := fake.NewBackend()
	bind := New(logger.NewLogger("test"), func(*config.Config) (ftpclient.Dialer, error) {
		return backend.Dialer(), nil
	})
	props := map[string]string{
		"address": "ftp.example.com:21", "username": "user", "password": "pass",
		"rootPath": "/upload",
	}
	meta := bindings.Metadata{Base: metadataBase(props)}

	for i := 0; i < 2; i++ {
		if err := bind.Init(context.Background(), meta); err != nil {
			t.Fatalf("Init %d: %v", i, err)
		}
		// Force the pool to actually open a session.
		if _, err := invoke(t, bind, bindings.ListOperation, nil, nil); err != nil {
			t.Fatalf("list after Init %d: %v", i, err)
		}
	}

	if err := bind.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := backend.LiveSessions(); live != 0 {
		t.Fatalf("%d sessions still open after Close; the pool from the first Init was never closed", live)
	}
}

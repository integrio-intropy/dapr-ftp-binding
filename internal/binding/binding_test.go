package binding

import (
	"context"
	"encoding/json"
	"io"
	"net/textproto"
	"slices"
	"strings"
	"sync"
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

// --- tempFileNamePattern ------------------------------------------------

// recordOps returns a FailNext spy that appends "op path" to ops and never
// fails, which is the cheapest way to assert the order of the wire operations.
func recordOps(backend *fake.Backend, ops *[]string) {
	var mu sync.Mutex
	backend.FailNext = func(op, p string) error {
		mu.Lock()
		defer mu.Unlock()
		*ops = append(*ops, op+" "+p)
		return nil
	}
}

// The default is off, so create stays a single STOR to the final name. This is
// the regression guard for every deployment that does not set the pattern.
func TestCreateWithoutTempPatternStoresDirectly(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, nil)
	var ops []string
	recordOps(backend, &ops)

	if _, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if want := []string{"store /upload/a.txt"}; !slices.Equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
}

func TestCreateWritesTempThenRenames(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, map[string]string{"tempFileNamePattern": "{name}.tmp"})
	var ops []string
	recordOps(backend, &ops)

	resp, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []string{"store /upload/a.txt.tmp", "rename /upload/a.txt.tmp"}
	if !slices.Equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
	if got, ok := backend.Get("/upload/a.txt"); !ok || string(got) != "hello" {
		t.Errorf("stored = %q (ok=%v), want hello", got, ok)
	}
	if paths := backend.Paths(); !slices.Equal(paths, []string{"/upload/a.txt"}) {
		t.Errorf("paths = %v, want only the final name with no temp left", paths)
	}
	// The caller is told the name it asked for, never the temp one.
	if resp.Metadata["fileName"] != "/upload/a.txt" {
		t.Errorf("response metadata fileName = %q", resp.Metadata["fileName"])
	}
}

func TestCreateCustomTempPattern(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, map[string]string{"tempFileNamePattern": ".{name}"})
	var ops []string
	recordOps(backend, &ops)

	if _, err := invoke(t, b, bindings.CreateOperation, []byte("x"), map[string]string{"fileName": "sub/a.txt"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The temp name stays in the final file's own directory.
	if want := []string{"store /upload/sub/.a.txt", "rename /upload/sub/.a.txt"}; !slices.Equal(ops, want) {
		t.Errorf("ops = %v, want %v", ops, want)
	}
	if paths := backend.Paths(); !slices.Equal(paths, []string{"/upload/sub/a.txt"}) {
		t.Errorf("paths = %v", paths)
	}
}

// A temp file left behind by an earlier crashed attempt is simply overwritten:
// STOR replaces, so unlike the SMB binding there is no state that blocks the
// next create of the same name.
func TestCreateOverwritesALeftoverTempFile(t *testing.T) {
	backend := fake.NewBackend()
	backend.Put("/upload/a.txt.tmp", []byte("partial junk from a crashed run"))
	b := newBinding(t, backend, map[string]string{"tempFileNamePattern": "{name}.tmp"})

	if _, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := backend.Get("/upload/a.txt"); string(got) != "hello" {
		t.Errorf("stored = %q, want hello", got)
	}
	if paths := backend.Paths(); !slices.Equal(paths, []string{"/upload/a.txt"}) {
		t.Errorf("paths = %v, want the leftover temp consumed", paths)
	}
}

// A failed rename must surface the name the caller asked for -- not the temp
// name, which means nothing to them -- and must not leave the temp behind.
func TestCreateCleansUpTheTempFileWhenRenameFails(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, map[string]string{"tempFileNamePattern": "{name}.tmp"})
	backend.FailNext = func(op, _ string) error {
		if op == "rename" {
			return &textproto.Error{Code: ftperr.CodeFileUnavailable, Msg: "Permission denied"}
		}
		return nil
	}

	_, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"})
	if err == nil {
		t.Fatal("expected the rename failure to surface")
	}
	for _, want := range []string{"create", "/upload/a.txt", "Permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), ".tmp") {
		t.Errorf("error %q names the temp file; the caller asked for the final name", err)
	}
	if paths := backend.Paths(); len(paths) != 0 {
		t.Errorf("paths = %v, want the temp file cleaned up", paths)
	}
}

// A server that refuses RNTO onto an existing name (553) is a command-level
// refusal: it is the server's answer, so it is reported rather than retried,
// and the temp file is cleaned up.
func TestCreateSurfacesARefusedOverwrite(t *testing.T) {
	backend := fake.NewBackend()
	backend.RenameRefusesOverwrite = true
	backend.Put("/upload/a.txt", []byte("existing"))
	b := newBinding(t, backend, map[string]string{"tempFileNamePattern": "{name}.tmp"})

	_, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"})
	if err == nil {
		t.Fatal("expected the 553 to surface")
	}
	if !strings.Contains(err.Error(), "553") {
		t.Errorf("error %q should carry the server's reply code", err)
	}
	if got, _ := backend.Get("/upload/a.txt"); string(got) != "existing" {
		t.Errorf("the existing file was modified: %q", got)
	}
	if paths := backend.Paths(); !slices.Equal(paths, []string{"/upload/a.txt"}) {
		t.Errorf("paths = %v, want the temp file cleaned up", paths)
	}
}

// The retry case from the design notes: the connection dies after the server
// has committed the rename but before its reply arrives. Because withConn
// re-runs the whole body, the second attempt's STOR recreates the temp file
// and the RNFR that follows finds it, so the operation still succeeds.
// Retrying the rename alone would have hit 550 on work that had been done.
func TestCreateRetriesTheWholeSequenceAfterAConnectionDrop(t *testing.T) {
	backend := fake.NewBackend()
	b := newBinding(t, backend, map[string]string{"tempFileNamePattern": "{name}.tmp"})

	before := backend.Dials()
	var dropped bool
	backend.FailNext = func(op, p string) error {
		if op == "rename" && !dropped {
			dropped = true
			// The server did the work; only the reply was lost.
			backend.Rename(p, "/upload/a.txt")
			return io.ErrUnexpectedEOF
		}
		return nil
	}

	if _, err := invoke(t, b, bindings.CreateOperation, []byte("hello"), map[string]string{"fileName": "a.txt"}); err != nil {
		t.Fatalf("create should have succeeded on the retry: %v", err)
	}
	if backend.Dials() <= before {
		t.Error("the retry should have dialled a fresh connection")
	}
	if got, _ := backend.Get("/upload/a.txt"); string(got) != "hello" {
		t.Errorf("stored = %q, want hello", got)
	}
	if paths := backend.Paths(); !slices.Equal(paths, []string{"/upload/a.txt"}) {
		t.Errorf("paths = %v, want no temp file left by the retry", paths)
	}
}

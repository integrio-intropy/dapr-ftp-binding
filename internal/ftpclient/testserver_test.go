package ftpclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

const (
	testUser = "user"
	testPass = "pass"
)

// testDriver is a minimal in-memory FTP server for exercising the adapter
// without Docker. The adapter is invisible to the fake-driven unit tests by
// construction, so this is the only tier that covers the dial func, the
// deadline tracker and the TLS wrapping.
type testDriver struct {
	fs        afero.Fs
	tlsConfig *tls.Config
	settings  *ftpserver.Settings

	// failReadAfter, when positive, makes a download yield that many bytes and
	// then fail. ftpserverlib sends its reply only after the data connection has
	// closed (TransferClose in handle_files.go), so the client sees a clean
	// stream and learns of the failure solely from the final status code. That
	// is the case the adapter must not swallow.
	failReadAfter int
}

func (d *testDriver) GetSettings() (*ftpserver.Settings, error) { return d.settings, nil }

func (d *testDriver) ClientConnected(ftpserver.ClientContext) (string, error) {
	return "test server ready", nil
}

func (d *testDriver) ClientDisconnected(ftpserver.ClientContext) {}

func (d *testDriver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != testUser || pass != testPass {
		return nil, errors.New("bad credentials")
	}
	return &clientDriver{Fs: d.fs, d: d}, nil
}

func (d *testDriver) GetTLSConfig() (*tls.Config, error) {
	if d.tlsConfig == nil {
		return nil, errors.New("TLS is not configured on this server")
	}
	return d.tlsConfig, nil
}

type clientDriver struct {
	afero.Fs
	d *testDriver
}

func (c *clientDriver) Open(name string) (afero.File, error) {
	f, err := c.Fs.Open(name)
	return c.wrap(f), err
}

func (c *clientDriver) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	f, err := c.Fs.OpenFile(name, flag, perm)
	if flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0 {
		return f, err // writes are untouched
	}
	return c.wrap(f), err
}

func (c *clientDriver) wrap(f afero.File) afero.File {
	if f == nil || c.d.failReadAfter <= 0 {
		return f
	}
	return &failingFile{File: f, remaining: c.d.failReadAfter}
}

// failingFile serves a prefix of the file and then errors, so the server aborts
// mid-transfer and reports it in the reply rather than on the data connection.
type failingFile struct {
	afero.File
	remaining int
}

func (f *failingFile) Read(p []byte) (int, error) {
	if f.remaining <= 0 {
		return 0, errors.New("simulated server-side read failure")
	}
	if len(p) > f.remaining {
		p = p[:f.remaining]
	}
	n, err := f.File.Read(p)
	f.remaining -= n
	return n, err
}

type serverOpts struct {
	tlsRequired ftpserver.TLSRequirement
	withTLS     bool
}

type testServer struct {
	addr   string
	fs     afero.Fs
	driver *testDriver
	caPEM  []byte
	stop   func()
}

func startServer(t *testing.T, opts serverOpts) *testServer {
	t.Helper()

	fs := afero.NewMemMapFs()
	if err := fs.MkdirAll("/upload", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	d := &testDriver{
		fs: fs,
		settings: &ftpserver.Settings{
			Listener:    lis,
			PublicHost:  "127.0.0.1",
			TLSRequired: opts.tlsRequired,
		},
	}

	var caPEM []byte
	if opts.withTLS {
		cert, ca := selfSignedCert(t)
		d.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		caPEM = ca
	}

	// ftpserverlib only wraps the listener for implicit TLS when it creates the
	// listener itself. We supply ours (to get an ephemeral port), so we wrap it.
	if opts.tlsRequired == ftpserver.ImplicitEncryption {
		d.settings.Listener = tls.NewListener(lis, d.tlsConfig)
	}

	srv := ftpserver.NewFtpServer(d)
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve()
	}()

	ts := &testServer{
		addr:   lis.Addr().String(),
		fs:     fs,
		driver: d,
		caPEM:  caPEM,
		stop: func() {
			_ = srv.Stop()
			<-done
		},
	}
	t.Cleanup(ts.stop)
	return ts
}

// clientTLS returns a tls.Config trusting the test server's certificate.
func (ts *testServer) clientTLS(t *testing.T) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ts.caPEM) {
		t.Fatal("could not parse the test CA")
	}
	return &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

func selfSignedCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	return cert, certPEM
}

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sdkbindings "github.com/dapr-sandbox/components-go-sdk/bindings/v1"
	contribbindings "github.com/dapr/components-contrib/bindings"
	"github.com/dapr/kit/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"

	"github.com/integrio-intropy/dapr-ftp-binding/internal/binding"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/config"
	"github.com/integrio-intropy/dapr-ftp-binding/internal/ftpclient"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// The sidecar and the Go SDK read DIFFERENT environment variables for the
	// socket directory, and they agree only on the default path:
	//
	//   daprd    DAPR_COMPONENTS_SOCKETS_FOLDER  (plural "components")
	//   Go SDK   DAPR_COMPONENT_SOCKETS_FOLDER   (singular), falling back to
	//            DAPR_COMPONENT_SOCKET_FOLDER
	//
	// Setting only the documented sidecar variable and using the SDK's own
	// runner puts the component in one directory and the sidecar's scan in
	// another, and the sidecar then reports "couldn't find binding". We honour
	// all three, preferring the sidecar's, so either spelling works.
	socketFolderEnvDapr   = "DAPR_COMPONENTS_SOCKETS_FOLDER"
	socketFolderEnvSDK    = "DAPR_COMPONENT_SOCKETS_FOLDER"
	socketFolderEnvLegacy = "DAPR_COMPONENT_SOCKET_FOLDER"
	defaultSocketFolder   = "/tmp/dapr-components-sockets"

	// instanceIDKey is the gRPC metadata the sidecar uses to distinguish
	// component instances sharing one socket.
	instanceIDKey     = "x-component-instance"
	defaultInstanceID = "__default__"

	// defaultMaxMessageSize is 4 MiB of payload plus headroom for protobuf
	// framing and response metadata. Raising it only helps uploads: the sidecar
	// dials pluggable components without raising gRPC's own 4 MiB receive limit,
	// so responses above that are rejected on its side regardless.
	defaultMaxMessageSize = (4 << 20) + (1 << 20)

	shutdownGrace = 10 * time.Second
)

func main() {
	var (
		socketName  = flag.String("socket-name", "ftp", "socket file name, without the .sock suffix; determines spec.type (bindings.<name>)")
		socketPerm  = flag.String("socket-perm", "0660", "permission bits for the socket file, in octal")
		maxMsgSize  = flag.Int("max-message-size", defaultMaxMessageSize, "maximum gRPC message size in bytes; raise alongside the sidecar's --max-body-size")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn or error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log := logger.NewLogger("dapr-ftp-binding")
	switch strings.ToLower(*logLevel) {
	case "debug":
		log.SetOutputLevel(logger.DebugLevel)
	case "info", "":
		log.SetOutputLevel(logger.InfoLevel)
	case "warn", "warning":
		log.SetOutputLevel(logger.WarnLevel)
	case "error":
		log.SetOutputLevel(logger.ErrorLevel)
	default:
		log.SetOutputLevel(logger.InfoLevel)
		log.Warnf("unrecognised log level %q, using info", *logLevel)
	}

	perm, err := parsePerm(*socketPerm)
	if err != nil {
		log.Fatalf("invalid -socket-perm: %v", err)
	}

	if err := run(log, *socketName, perm, *maxMsgSize); err != nil {
		log.Fatalf("ftp binding failed: %v", err)
	}
}

// parsePerm reads permission bits as octal, whether or not they carry a leading
// zero. Taking them as a string rather than via flag.Uint is deliberate: that
// helper parses with a base of 0, so a perfectly reasonable "-socket-perm 660"
// would be read as decimal 660, which is 0o1224 -- setgid plus mode 0224.
func parsePerm(s string) (os.FileMode, error) {
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an octal mode, for example \"0660\"", s)
	}
	if n > 0o777 {
		return 0, fmt.Errorf("mode %#o is wider than 0777", n)
	}
	return os.FileMode(n), nil
}

func run(log logger.Logger, socketName string, perm os.FileMode, maxMsgSize int) error {
	socketPath := filepath.Join(socketFolder(), socketName+".sock")

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return fmt.Errorf("creating the socket directory %s: %w", filepath.Dir(socketPath), err)
	}
	// A socket left behind by a previous run would make Listen fail.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the stale socket %s: %w", socketPath, err)
	}

	// Anything able to write to this socket can drive the component, so create it
	// with restrictive permissions rather than fixing them afterwards.
	//
	// The umask is what decides the mode at creation time; Chmod afterwards
	// leaves a brief window at whatever the umask allowed. Under the usual 022
	// that window is 0755, and since connecting to a Unix socket requires the
	// write bit, others cannot use it even then. Narrowing it anyway costs
	// nothing and means this does not silently become a real window if the
	// process umask is ever more permissive -- for instance if someone imports
	// the Dapr Go SDK's root package, whose init() calls syscall.Umask(0). (This
	// binary does not import it: it uses bindings/v1 directly, which is why we
	// are managing the socket ourselves in the first place.)
	var lis net.Listener
	var err error
	withUmask(0o117, func() { // 0777 &^ 0117 == 0660
		lis, err = net.Listen("unix", socketPath)
	})
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	// Belt and braces: the umask is ignored on some filesystems, notably Docker
	// Desktop bind mounts on macOS, where this Chmod also fails and we warn.
	if err := os.Chmod(socketPath, perm); err != nil {
		log.Warnf("could not set permissions on %s: %v", socketPath, err)
	}

	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
	)

	reg := newRegistry(log)
	sdkbindings.RegisterOutput(server, reg.get)

	// REQUIRED: the Dapr sidecar discovers what a pluggable component implements
	// by calling the gRPC reflection API on the socket (see
	// pkg/components/pluggable/discovery.go, which calls ListServices). Without
	// this the component starts, the socket appears, and the sidecar still fails
	// with "couldn't find binding ftp (bindings.ftp/v1)".
	reflection.Register(server)

	log.Infof("dapr-ftp-binding %s listening on %s (spec.type: bindings.%s, max message size %d bytes)",
		version, socketPath, socketName, maxMsgSize)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(lis) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		reg.closeAll()
		return err
	case s := <-sig:
		log.Infof("received %s, shutting down", s)
	}

	// Stop accepting work, then close every pool so the FTP server sees a
	// proper QUIT rather than a dropped connection. The SDK provides no hook
	// for this, which is the other reason we run our own listener.
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownGrace):
		log.Warn("graceful shutdown timed out, forcing stop")
		server.Stop()
	}
	reg.closeAll()
	log.Info("shutdown complete")
	return nil
}

func socketFolder() string {
	for _, env := range []string{socketFolderEnvDapr, socketFolderEnvSDK, socketFolderEnvLegacy} {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			return v
		}
	}
	return defaultSocketFolder
}

// registry caches one component instance per sidecar instance id, mirroring
// the SDK's own multiplexer, but keeping a handle on each so shutdown can close
// them. The SDK's version never evicts and never closes.
type registry struct {
	log logger.Logger

	mu        sync.Mutex
	instances map[string]*binding.Binding
}

func newRegistry(log logger.Logger) *registry {
	return &registry{log: log, instances: map[string]*binding.Binding{}}
}

func (r *registry) get(ctx context.Context) sdkbindings.OutputBinding {
	id := defaultInstanceID
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(instanceIDKey); len(vals) > 0 && vals[0] != "" {
			id = vals[0]
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.instances[id]; ok {
		return b
	}
	b := binding.New(r.log, newDialer)
	r.instances[id] = b
	r.log.Debugf("created component instance %q (%d live)", id, len(r.instances))
	return b
}

func (r *registry) closeAll() {
	r.mu.Lock()
	instances := r.instances
	r.instances = map[string]*binding.Binding{}
	r.mu.Unlock()

	for id, b := range instances {
		if err := b.Close(); err != nil {
			r.log.Warnf("closing component instance %q: %v", id, err)
		}
	}
}

// newDialer bridges the parsed configuration to the FTP adapter.
func newDialer(cfg *config.Config) (ftpclient.Dialer, error) {
	mode, err := tlsMode(cfg.TLSMode)
	if err != nil {
		return nil, err
	}
	return ftpclient.NewDialer(ftpclient.DialerConfig{
		Address:     cfg.Address,
		Username:    cfg.Username,
		Password:    cfg.Password,
		TLSMode:     mode,
		TLSConfig:   cfg.TLSConfig,
		Timeout:     cfg.Timeout,
		DisableEPSV: cfg.DisableEPSV,
		TrustPASVIP: cfg.TrustPASVIP,
		TargetName:  cfg.Target(),
	}), nil
}

// tlsMode translates between the two enumerations rather than converting them
// numerically. The adapter deliberately does not import config, so nothing but
// this function stops the two from drifting apart silently — and the failure
// mode of drift is a connection that is weaker than configured.
func tlsMode(m config.TLSMode) (ftpclient.TLSMode, error) {
	switch m {
	case config.TLSNone:
		return ftpclient.TLSNone, nil
	case config.TLSExplicit:
		return ftpclient.TLSExplicit, nil
	case config.TLSImplicit:
		return ftpclient.TLSImplicit, nil
	default:
		return 0, fmt.Errorf("unknown TLS mode %d", m)
	}
}

// Compile-time proof that the component satisfies both the SDK and contrib
// interfaces; the contrib one requires GetComponentMetadata, which the Dapr
// docs page for Go bindings omits.
var (
	_ sdkbindings.OutputBinding     = (*binding.Binding)(nil)
	_ contribbindings.OutputBinding = (*binding.Binding)(nil)
)

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/dapr/kit/logger"
)

// rejoinWatcher restarts a sidecar that loaded this binding before we were
// running.
//
// daprd connects to a pluggable component's socket once, while it loads its
// components, and never reconnects. When this container restarts on its own (an
// OOM kill, a crash) and daprd keeps running, daprd still holds the old, dead
// connection: every call fails with "error reading from server: EOF" until
// daprd itself restarts. In a Deployment the pod looks healthy, so nothing
// would ever restart it.
//
// So at startup we look for exactly that state: daprd is up (its HTTP API
// answers, which it opens only once all its components have loaded), its
// metadata lists a component of our type, and it has not called our Init. Then it
// loaded us before we were here, and we ask it to shut down. Kubernetes
// restarts it in the same pod (restartPolicy Always or, after an error,
// OnFailure), and the new daprd connects to us.
//
// The metadata check keeps us from shutting down a daprd that never meant to
// load us, e.g. one whose app is not in the Component's scopes.
type rejoinWatcher struct {
	componentType string
	// initialised is set on daprd's first Init call to this process.
	initialised *atomic.Bool

	// grace lets an Init that is on its way arrive first; giveUp ends the watch,
	// since by then daprd has either connected to us or is not coming.
	grace, giveUp, interval time.Duration

	// Fields so tests can substitute fakes for daprd's HTTP API.
	metadata    func() (*daprMetadata, error)
	stopSidecar func() error
}

type daprMetadata struct {
	Components []struct {
		Type string `json:"type"`
	} `json:"components"`
}

const (
	defaultRejoinGrace    = 10 * time.Second
	defaultRejoinGiveUp   = 2 * time.Minute
	defaultRejoinInterval = 2 * time.Second
)

func newRejoinWatcher(componentType string, initialised *atomic.Bool) *rejoinWatcher {
	base := "http://" + net.JoinHostPort("127.0.0.1", daprHTTPPort())
	client := &http.Client{Timeout: 5 * time.Second}
	return &rejoinWatcher{
		componentType: componentType,
		initialised:   initialised,
		grace:         defaultRejoinGrace,
		giveUp:        defaultRejoinGiveUp,
		interval:      defaultRejoinInterval,
		metadata: func() (*daprMetadata, error) {
			resp, err := client.Get(base + "/v1.0/metadata")
			if err != nil {
				return nil, err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("metadata: %s", resp.Status)
			}
			var md daprMetadata
			return &md, json.NewDecoder(resp.Body).Decode(&md)
		},
		stopSidecar: func() error {
			resp, err := client.Post(base+"/v1.0/shutdown", "", nil)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				return fmt.Errorf("shutdown: %s", resp.Status)
			}
			return nil
		},
	}
}

// watch asks daprd to shut down if it loaded our type before we started, and
// reports whether it did.
func (w *rejoinWatcher) watch(log logger.Logger) bool {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	start := time.Now()
	for range t.C {
		if w.initialised.Load() {
			return false
		}
		elapsed := time.Since(start)
		if elapsed >= w.giveUp {
			return false
		}
		if elapsed < w.grace {
			continue
		}
		md, err := w.metadata()
		if err != nil {
			continue // daprd is not up (yet)
		}
		if !md.has(w.componentType) {
			return false // daprd runs without us on purpose
		}
		log.Warnf("daprd loaded %s before this binding was running and holds a dead connection to it; "+
			"asking it to shut down so it restarts and reconnects", w.componentType)
		if err := w.stopSidecar(); err != nil {
			log.Errorf("could not shut daprd down: %v", err)
		}
		return true
	}
	return false
}

func (md *daprMetadata) has(componentType string) bool {
	for _, c := range md.Components {
		if c.Type == componentType {
			return true
		}
	}
	return false
}

func daprHTTPPort() string {
	if port := os.Getenv(daprHTTPPortEnv); port != "" {
		return port
	}
	return defaultDaprHTTPPort
}

package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	contribbindings "github.com/dapr/components-contrib/bindings"
	"github.com/dapr/kit/logger"
)

// fakeDaprd stands in for daprd's metadata and shutdown endpoints.
type fakeDaprd struct {
	types    []string // nil: daprd is not up
	stopped  atomic.Int32
	initDone *atomic.Bool
}

func (f *fakeDaprd) watcher() *rejoinWatcher {
	f.initDone = &atomic.Bool{}
	w := newRejoinWatcher("bindings.ftp", f.initDone)
	w.grace, w.giveUp, w.interval = 3*time.Millisecond, 100*time.Millisecond, time.Millisecond
	w.metadata = func() (*daprMetadata, error) {
		if f.types == nil {
			return nil, errors.New("connection refused")
		}
		md := &daprMetadata{}
		for _, t := range f.types {
			md.Components = append(md.Components, struct {
				Type string `json:"type"`
			}{t})
		}
		return md, nil
	}
	w.stopSidecar = func() error { f.stopped.Add(1); return nil }
	return w
}

var testLog = logger.NewLogger("test")

func TestRejoinStopsDaprdThatLoadedUsBeforeWeStarted(t *testing.T) {
	f := &fakeDaprd{types: []string{"state.redis", "bindings.ftp"}}
	if !f.watcher().watch(testLog) || f.stopped.Load() != 1 {
		t.Fatalf("expected daprd to be asked to shut down once, got %d", f.stopped.Load())
	}
}

func TestRejoinLeavesDaprdAloneOnceItCalledInit(t *testing.T) {
	f := &fakeDaprd{types: []string{"bindings.ftp"}}
	w := f.watcher()
	f.initDone.Store(true)
	if w.watch(testLog) || f.stopped.Load() != 0 {
		t.Fatal("daprd that called Init must not be shut down")
	}
}

func TestRejoinLeavesDaprdAloneWhenItDoesNotUseUs(t *testing.T) {
	f := &fakeDaprd{types: []string{"bindings.smb"}}
	if f.watcher().watch(testLog) || f.stopped.Load() != 0 {
		t.Fatal("daprd without our component type must not be shut down")
	}
}

func TestRejoinGivesUpWhenDaprdNeverAnswers(t *testing.T) {
	f := &fakeDaprd{}
	if f.watcher().watch(testLog) || f.stopped.Load() != 0 {
		t.Fatal("no daprd, nothing to shut down")
	}
}

func TestRejoinWaitsForGraceBeforeAsking(t *testing.T) {
	f := &fakeDaprd{types: []string{"bindings.ftp"}}
	w := f.watcher()
	w.grace = time.Hour
	if w.watch(testLog) || f.stopped.Load() != 0 {
		t.Fatal("must not act within the grace period")
	}
}

func TestTrackedBindingRecordsInit(t *testing.T) {
	r := newRegistry(testLog)
	b := r.get(t.Context())
	if r.initialised.Load() {
		t.Fatal("getting an instance is not Init")
	}
	_ = b.Init(t.Context(), contribbindings.Metadata{}) // fails on empty metadata; still an Init
	if !r.initialised.Load() {
		t.Fatal("Init must be recorded")
	}
}

package main

import (
	"errors"
	"math"
	"runtime/debug"
	"testing"
)

func TestSetMemoryLimit(t *testing.T) {
	defer debug.SetMemoryLimit(math.MaxInt64)
	file := func(s string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(s), nil }
	}

	if limit, ok := setMemoryLimit(file("134217728\n")); !ok || limit != 107374182 {
		t.Fatalf("128Mi container: got %d, %v; want 80%% of it", limit, ok)
	}
	if debug.SetMemoryLimit(-1) != 107374182 {
		t.Fatal("the runtime's limit was not set")
	}
	if _, ok := setMemoryLimit(file("max\n")); ok {
		t.Fatal("no container limit must leave the runtime alone")
	}
	if _, ok := setMemoryLimit(func(string) ([]byte, error) { return nil, errors.New("no cgroup v2") }); ok {
		t.Fatal("no cgroup file must leave the runtime alone")
	}
	t.Setenv("GOMEMLIMIT", "50MiB")
	if _, ok := setMemoryLimit(file("134217728\n")); ok {
		t.Fatal("GOMEMLIMIT must win")
	}
}

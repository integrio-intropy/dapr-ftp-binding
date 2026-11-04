package main

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// cgroupMemoryMax is where cgroup v2 exposes the container's memory limit.
const cgroupMemoryMax = "/sys/fs/cgroup/memory.max"

// memoryLimitShare is the share of the container's limit the Go heap may use,
// leaving the rest for goroutine stacks, the runtime and TLS buffers.
const memoryLimitShare = 0.8

// setMemoryLimit tells the Go runtime the container's memory limit, unless
// GOMEMLIMIT already does.
//
// Without it the garbage collector lets the heap grow to twice what is live
// before collecting. A 16 MiB file is held two or three times over during a
// call (the gRPC message, the transfer buffer, the response), so a few calls in
// a row pass a 128Mi limit and the container is OOM-killed, although far less
// than that is ever live. With the limit the collector runs before that.
func setMemoryLimit(read func(string) ([]byte, error)) (int64, bool) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0, false
	}
	raw, err := read(cgroupMemoryMax)
	if err != nil {
		return 0, false // not Linux, or cgroup v1: leave the runtime's default
	}
	containerMax, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || containerMax <= 0 {
		return 0, false // "max": no limit
	}
	limit := int64(float64(containerMax) * memoryLimitShare)
	debug.SetMemoryLimit(limit)
	return limit, true
}

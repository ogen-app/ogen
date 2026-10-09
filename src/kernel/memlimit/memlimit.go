// Package memlimit sets the Go runtime's soft memory limit from the
// container's cgroup memory limit.
//
// Without a limit the GC lets the heap grow to twice the live set before
// collecting, so a burst of large uploads or document parses can push the
// process past the container limit and get it OOM-killed even though most of
// that heap is garbage. A soft limit just below the container's makes the GC
// work harder as the heap nears it instead.
package memlimit

import (
	"bytes"
	"errors"
	"os"
	"runtime/debug"
	"strconv"
)

// headroom is the share of the container limit given to the Go heap; the rest
// covers goroutine stacks and other non-heap runtime memory.
const headroom = 0.9

// cgroupFiles are the memory-limit files of cgroup v2 and v1, in that order.
var cgroupFiles = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

// Apply sets the soft memory limit to headroom × the cgroup limit and returns
// the limit it set. It does nothing (returning 0) when GOMEMLIMIT is set
// explicitly, or when no cgroup limit is found, as outside a container.
func Apply() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	for _, f := range cgroupFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		limit, err := parse(raw)
		if err != nil {
			continue
		}
		soft := int64(float64(limit) * headroom)
		debug.SetMemoryLimit(soft)
		return soft
	}
	return 0
}

// errNoLimit reports an unlimited cgroup ("max" in v2, or v1's page-aligned
// sentinel near MaxInt64).
var errNoLimit = errors.New("no cgroup memory limit")

// noLimitThreshold treats anything above 1 PiB as v1's "unlimited" sentinel.
const noLimitThreshold = 1 << 50

func parse(raw []byte) (int64, error) {
	s := string(bytes.TrimSpace(raw))
	if s == "max" {
		return 0, errNoLimit
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 || n >= noLimitThreshold {
		return 0, errNoLimit
	}
	return n, nil
}

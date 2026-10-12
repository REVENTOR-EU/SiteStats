package collect

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DiskUsage returns the size of a directory in bytes via `du`, niced so it
// never steals CPU from production workloads.
func DiskUsage(ctx context.Context, dir string, timeout time.Duration) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "nice", "-n", "19", "du", "-s", "-B1", dir)
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := cctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, err
	}
	return strconv.ParseInt(fields[0], 10, 64)
}

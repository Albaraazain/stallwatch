package collector

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// execCollector runs a command and parses its stdout as a number. This one
// collector already covers ssh, docker exec, psql, redis-cli, jq pipelines —
// anything that can print a value — without stallwatch needing native drivers.
type execCollector struct {
	cmd []string
}

func (c *execCollector) Collect(ctx context.Context) (float64, error) {
	cmd := exec.CommandContext(ctx, c.cmd[0], c.cmd[1:]...)
	// CommandContext only kills the direct child on cancellation; a probe
	// like `sh -c "ssh host ..."` leaves grandchildren holding the stdout
	// pipe, which blocks Output() past the deadline and leaks processes.
	// Run the probe in its own process group and kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second // backstop if the group kill fails
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return 0, fmt.Errorf("%s: %w: %s", c.cmd[0], err, firstLine(exitErr.Stderr))
		}
		return 0, fmt.Errorf("%s: %w", c.cmd[0], err)
	}
	text := strings.TrimSpace(string(out))
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: output %q is not numeric", c.cmd[0], truncate(text, 80))
	}
	return v, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

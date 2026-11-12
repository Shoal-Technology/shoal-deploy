package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"
)

func newPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

func readLines(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// dockerRun executes docker and returns trimmed stdout.
func dockerRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr == "" {
			stderr = strings.TrimSpace(string(out))
		}
		if stderr == "" {
			stderr = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

func dockerAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := dockerRun(ctx, "version", "--format", "{{.Server.Version}}")
	return err == nil
}

func dockerVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := dockerRun(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "unavailable"
	}
	return out
}

// containerPort asks Docker which loopback port was published for a container port.
func containerPort(ctx context.Context, name string, port int) (int, error) {
	out, err := dockerRun(ctx, "port", name, fmt.Sprintf("%d/tcp", port))
	if err != nil {
		return 0, err
	}
	first := strings.Split(strings.TrimSpace(out), "\n")[0]
	_, p, err := net.SplitHostPort(strings.TrimSpace(first))
	if err != nil {
		return 0, fmt.Errorf("docker port output %q: %w", out, err)
	}
	var n int
	if _, err := fmt.Sscan(p, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// containerLogs keeps both streams: applications report their failures on
// stderr, and docker writes the container's stderr to its own stderr — the
// stdout-only path in dockerRun threw exactly those lines away.
func containerLogs(ctx context.Context, name string, tail int) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", fmt.Sprint(tail), name).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

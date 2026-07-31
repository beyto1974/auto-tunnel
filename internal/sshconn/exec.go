package sshconn

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// RunCommand executes cmd on the remote host and returns its stdout. A non-zero
// exit turns into an error carrying the trimmed stderr, so callers can surface a
// readable reason ("permission denied while trying to connect to the Docker
// daemon") instead of a bare exit status.
func RunCommand(ctx context.Context, client *ssh.Client, cmd string) ([]byte, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open ssh session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return stdout.Bytes(), fmt.Errorf("remote command %q failed: %s", cmd, firstLines(msg, 3))
			}
			return stdout.Bytes(), fmt.Errorf("remote command %q failed: %w", cmd, err)
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.Join(lines, "; ")
	}
	return strings.Join(lines[:n], "; ") + " ..."
}

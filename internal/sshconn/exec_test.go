package sshconn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beyto1974/auto-tunnel/internal/sshtest"
)

func TestRunCommandReturnsStdout(t *testing.T) {
	srv := newTrustedServer(t)
	srv.SetExec(func(cmd string) sshtest.ExecResult {
		if cmd != "docker ps" {
			return sshtest.ExecResult{Stderr: "unexpected command " + cmd, Exit: 127}
		}
		return sshtest.ExecResult{Stdout: `{"ID":"abc","Names":"web"}` + "\n"}
	})

	client := dialFixture(t, srv)

	out, err := RunCommand(t.Context(), client, "docker ps")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if got, want := string(out), `{"ID":"abc","Names":"web"}`+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunCommandSurfacesRemoteStderr(t *testing.T) {
	// The bare exit status ("Process exited with status 1") tells the user
	// nothing; the daemon's own explanation is the whole point of the error.
	srv := newTrustedServer(t)
	srv.SetExec(func(string) sshtest.ExecResult {
		return sshtest.ExecResult{
			Stderr: "permission denied while trying to connect to the Docker daemon socket",
			Exit:   1,
		}
	})

	client := dialFixture(t, srv)

	_, err := RunCommand(t.Context(), client, "docker ps")
	if err == nil {
		t.Fatal("RunCommand succeeded, want the remote failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %v, want it to carry the remote stderr", err)
	}
}

func TestRunCommandTrimsALongStderr(t *testing.T) {
	// A remote command can print pages of context; the dashboard banner and the
	// log line both have one line to work with.
	srv := newTrustedServer(t)
	srv.SetExec(func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stderr: "line one\nline two\nline three\nline four\nline five", Exit: 2}
	})

	client := dialFixture(t, srv)

	_, err := RunCommand(t.Context(), client, "docker ps")
	if err == nil {
		t.Fatal("RunCommand succeeded, want the remote failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line one; line two; line three ...") {
		t.Errorf("error = %q, want the first three lines joined and elided", msg)
	}
	if strings.Contains(msg, "line four") {
		t.Errorf("error = %q, want the tail dropped", msg)
	}
}

func TestRunCommandReportsAFailureWithNoStderr(t *testing.T) {
	srv := newTrustedServer(t)
	srv.SetExec(func(string) sshtest.ExecResult { return sshtest.ExecResult{Exit: 1} })

	client := dialFixture(t, srv)

	_, err := RunCommand(t.Context(), client, "docker ps")
	if err == nil {
		t.Fatal("RunCommand succeeded, want the non-zero exit reported")
	}
	if !strings.Contains(err.Error(), `remote command "docker ps" failed`) {
		t.Errorf("error = %v, want it to name the command", err)
	}
}

func TestRunCommandHonoursContextCancellation(t *testing.T) {
	// A poll must not outlive the shutdown that cancelled it, however long the
	// remote command decides to take.
	srv := newTrustedServer(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.SetExec(func(string) sshtest.ExecResult {
		<-release
		return sshtest.ExecResult{}
	})

	client := dialFixture(t, srv)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := RunCommand(ctx, client, "sleep forever"); done <- err }()

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunCommand returned no error after its context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCommand ignored its cancelled context")
	}
}

func TestRunCommandReportsAnUnusableConnection(t *testing.T) {
	srv := newTrustedServer(t)
	client := dialFixture(t, srv)
	client.Close()

	if _, err := RunCommand(t.Context(), client, "docker ps"); err == nil {
		t.Fatal("RunCommand succeeded on a closed client, want an error")
	} else if !strings.Contains(err.Error(), "open ssh session") {
		t.Errorf("error = %v, want it to name the failed step", err)
	}
}

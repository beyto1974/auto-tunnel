package sshtest

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A broken fixture shows up as a confusing failure in three other packages, so
// it is worth testing on its own terms. These tests dial with x/crypto/ssh
// directly rather than through sshconn, which keeps the dependency one-way.

func TestMain(m *testing.M) {
	// Helpers here write under $HOME/.ssh; point that somewhere disposable so a
	// run never touches the developer's real SSH configuration.
	home, err := os.MkdirTemp("", "sshtest-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// dial is Dial, named locally so the tests below read the same as a caller's.
func dial(t *testing.T, srv *Server) *ssh.Client { return Dial(t, srv) }

func run(t *testing.T, client *ssh.Client, cmd string) (string, string, error) {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	runErr := session.Run(cmd)
	return stdout.String(), stderr.String(), runErr
}

func TestServerRunsCommands(t *testing.T) {
	srv := New(t)
	srv.SetExec(func(cmd string) ExecResult {
		return ExecResult{Stdout: "ran " + cmd + "\n"}
	})
	client := dial(t, srv)

	stdout, _, err := run(t, client, "docker ps")

	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout != "ran docker ps\n" {
		t.Errorf("stdout = %q, want the handler's output", stdout)
	}
	if got := srv.Commands(); len(got) != 1 || got[0] != "docker ps" {
		t.Errorf("Commands() = %v, want the one command that ran", got)
	}
}

func TestServerReportsStderrAndExitStatus(t *testing.T) {
	srv := New(t)
	srv.SetExec(func(string) ExecResult {
		return ExecResult{Stderr: "permission denied\n", Exit: 13}
	})
	client := dial(t, srv)

	_, stderr, err := run(t, client, "docker ps")

	if err == nil {
		t.Fatal("run succeeded, want the non-zero exit reported")
	}
	var exit *ssh.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("error = %v (%T), want an *ssh.ExitError", err, err)
	}
	if exit.ExitStatus() != 13 {
		t.Errorf("exit status = %d, want 13", exit.ExitStatus())
	}
	if stderr != "permission denied\n" {
		t.Errorf("stderr = %q, want the handler's error output", stderr)
	}
}

func TestServerAnswersKeepalives(t *testing.T) {
	srv := New(t)
	if srv.Port() == 0 {
		t.Fatal("Port() = 0, want the loopback port the fixture bound")
	}
	client := dial(t, srv)

	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if got := srv.PingCount(); got != 1 {
		t.Errorf("PingCount() = %d, want 1", got)
	}
}

func TestServerCanStallKeepalives(t *testing.T) {
	// This is what lets a caller test the keepalive timeout: the request arrives
	// and simply is not answered.
	srv := New(t)
	srv.SetKeepaliveGap(300 * time.Millisecond)
	client := dial(t, srv)

	start := time.Now()
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("keepalive: %v", err)
	}

	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("keepalive answered after %s, want it held for at least 300ms", elapsed)
	}
}

func TestServerForwardsDirectTCPIP(t *testing.T) {
	// The forwarder's whole job runs over direct-tcpip channels, so the fixture
	// has to carry them to a real local listener.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	srv := New(t)
	client := dial(t, srv)

	remote, err := client.Dial("tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("direct-tcpip dial: %v", err)
	}
	defer remote.Close()
	remote.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(remote, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(remote, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want %q", buf, "ping")
	}
}

func TestServerRejectsAnUnreachableForwardTarget(t *testing.T) {
	srv := New(t)
	client := dial(t, srv)

	dead := net.JoinHostPort("127.0.0.1", strconv.Itoa(ClosedPort(t)))
	if _, err := client.Dial("tcp", dead); err == nil {
		t.Fatal("direct-tcpip to a closed port succeeded, want a rejection")
	}
}

func TestServerRejectsUnknownChannelTypes(t *testing.T) {
	srv := New(t)
	client := dial(t, srv)

	if _, _, err := client.OpenChannel("x11", nil); err == nil {
		t.Fatal("the fixture accepted an x11 channel, want a rejection")
	}
}

func TestDropConnectionsEndsLiveSessions(t *testing.T) {
	srv := New(t)
	client := dial(t, srv)
	srv.WaitForConnection(t)

	if got := srv.ConnCount(); got != 1 {
		t.Fatalf("ConnCount() = %d, want 1", got)
	}

	srv.DropConnections()

	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never noticed the dropped connection")
	}
	if got := srv.ConnCount(); got != 0 {
		t.Errorf("ConnCount() = %d after dropping, want 0", got)
	}
}

func TestCloseIsIdempotentAndStopsAccepting(t *testing.T) {
	srv := New(t)
	addr := srv.Addr()

	srv.Close()
	srv.Close() // the cleanup registered by New will call it a third time

	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Error("the fixture still accepts connections after Close")
	}
}

func TestTrustWritesKnownHostsAndClearsTheAgentSocket(t *testing.T) {
	first := New(t)
	second := New(t)

	Trust(t, first, second)

	if got := os.Getenv("SSH_AUTH_SOCK"); got != "" {
		t.Errorf("SSH_AUTH_SOCK = %q, want it cleared", got)
	}

	path := filepath.Join(SSHDir(t), "known_hosts")
	callback, err := knownhosts.New(path)
	if err != nil {
		t.Fatalf("parse the known_hosts we wrote: %v", err)
	}
	for _, srv := range []*Server{first, second} {
		if err := callback(srv.Addr(), srv.NetAddr(), srv.HostKey()); err != nil {
			t.Errorf("known_hosts rejects %s: %v", srv.Addr(), err)
		}
	}
}

func TestInstallDefaultKeyWritesTheConventionalPath(t *testing.T) {
	srv := New(t)

	InstallDefaultKey(t, srv)

	path := filepath.Join(SSHDir(t), "id_ed25519")
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("parse the installed key: %v", err)
	}
	if !strings.EqualFold(signer.PublicKey().Type(), "ssh-ed25519") {
		t.Errorf("installed key type = %q, want ssh-ed25519", signer.PublicKey().Type())
	}
}

func TestStartAgentServesTheKeysItWasGiven(t *testing.T) {
	srv := New(t)
	StartAgent(t, srv.ClientKey())

	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		t.Fatal("StartAgent did not set SSH_AUTH_SOCK")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial the agent: %v", err)
	}
	defer conn.Close()

	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		t.Fatalf("list agent keys: %v", err)
	}
	if len(signers) != 1 {
		t.Fatalf("agent holds %d keys, want 1", len(signers))
	}
	if got, want := signers[0].PublicKey().Marshal(), srv.HostKey().Marshal(); string(got) == string(want) {
		t.Error("the agent holds the host key, want the client key")
	}
}

func TestStartAgentCanBeEmpty(t *testing.T) {
	StartAgent(t)

	conn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		t.Fatalf("dial the agent: %v", err)
	}
	defer conn.Close()

	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		t.Fatalf("list agent keys: %v", err)
	}
	if len(signers) != 0 {
		t.Errorf("agent holds %d keys, want none", len(signers))
	}
}

func TestMarshalPrivateKeyWithAndWithoutAPassphrase(t *testing.T) {
	key := GenerateKey(t)

	if _, err := ssh.ParsePrivateKey(MarshalPrivateKey(t, key.Private, "")); err != nil {
		t.Errorf("parse an unencrypted key: %v", err)
	}

	encrypted := MarshalPrivateKey(t, key.Private, "hunter2")
	if _, err := ssh.ParsePrivateKey(encrypted); err == nil {
		t.Error("an encrypted key parsed without its passphrase")
	}
	if _, err := ssh.ParsePrivateKeyWithPassphrase(encrypted, []byte("hunter2")); err != nil {
		t.Errorf("parse an encrypted key with its passphrase: %v", err)
	}
}

// asExitError is errors.As specialised to keep the test readable.
func asExitError(err error, target **ssh.ExitError) bool {
	e, ok := err.(*ssh.ExitError)
	if ok {
		*target = e
	}
	return ok
}

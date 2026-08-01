// Package sshtest provides an in-process SSH server for tests.
//
// Three packages need a live *ssh.Client to be testable at all — sshconn dials
// one, discovery runs commands over one, and the engine wires both together —
// and none of them can be exercised with a fake, because the standard library
// type is concrete. Running a real server instead means those paths are tested
// as they actually behave, handshake included.
//
// Nothing here is imported by the command itself; it exists only for tests, in
// the same spirit as net/http/httptest.
package sshtest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ExecResult is what the fixture's shell "returns" for one command.
type ExecResult struct {
	Stdout string
	Stderr string
	Exit   uint32
}

// Server accepts SSH connections on loopback and answers exec, keepalive and
// direct-tcpip requests. Everything guarded by mu can be changed mid-test.
type Server struct {
	listener      net.Listener
	hostKey       ssh.Signer
	clientKey     ssh.Signer
	clientKeyPriv ed25519.PrivateKey // the same key, for loading into a test agent
	keyPath       string             // the client private key, in the test's temp dir

	mu           sync.Mutex
	exec         func(cmd string) ExecResult
	keepaliveGap time.Duration // how long to sit on a keepalive before replying
	pings        int
	commands     []string
	conns        []*ssh.ServerConn
	closed       bool
}

// New starts a server that answers every exec with empty output and stops when
// the test ends.
func New(t *testing.T) *Server {
	t.Helper()

	hostKey := GenerateKey(t)
	clientKey := GenerateKey(t)

	srv := &Server{
		hostKey:       hostKey.Signer,
		clientKey:     clientKey.Signer,
		clientKeyPriv: clientKey.Private,
		exec:          func(string) ExecResult { return ExecResult{} },
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientKey.Signer.PublicKey().Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown public key")
		},
	}
	cfg.AddHostKey(hostKey.Signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = ln
	t.Cleanup(srv.Close)

	go srv.acceptLoop(cfg)

	srv.keyPath = writeClientKey(t, clientKey)
	return srv
}

func (s *Server) acceptLoop(cfg *ssh.ServerConfig) {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serve(nc, cfg)
	}
}

func (s *Server) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}

	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()

	go s.handleGlobalRequests(reqs)
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go s.handleSession(newChan)
		case "direct-tcpip":
			go s.handleDirectTCPIP(newChan)
		default:
			newChan.Reject(ssh.UnknownChannelType, newChan.ChannelType())
		}
	}
	conn.Close()
}

func (s *Server) handleGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type == "keepalive@openssh.com" {
			s.mu.Lock()
			s.pings++
			gap := s.keepaliveGap
			s.mu.Unlock()
			if gap > 0 {
				// A black-holed link: the request arrives and is never answered.
				time.Sleep(gap)
			}
		}
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
}

func (s *Server) handleSession(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				return
			}
			req.Reply(true, nil)

			s.mu.Lock()
			s.commands = append(s.commands, payload.Command)
			handler := s.exec
			s.mu.Unlock()
			res := handler(payload.Command)

			io.WriteString(ch, res.Stdout)
			io.WriteString(ch.Stderr(), res.Stderr)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{res.Exit}))
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *Server) handleDirectTCPIP(newChan ssh.NewChannel) {
	var payload struct {
		DestAddr string
		DestPort uint32
		SrcAddr  string
		SrcPort  uint32
	}
	if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
		newChan.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	target := net.JoinHostPort(payload.DestAddr, strconv.Itoa(int(payload.DestPort)))
	remote, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		remote.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		defer remote.Close()
		defer ch.Close()
		io.Copy(remote, ch)
	}()
	go func() {
		defer remote.Close()
		defer ch.Close()
		io.Copy(ch, remote)
	}()
}

// SetExec installs the shell the fixture pretends to have.
func (s *Server) SetExec(fn func(cmd string) ExecResult) {
	s.mu.Lock()
	s.exec = fn
	s.mu.Unlock()
}

// SetKeepaliveGap makes keepalive requests hang for d before being answered.
func (s *Server) SetKeepaliveGap(d time.Duration) {
	s.mu.Lock()
	s.keepaliveGap = d
	s.mu.Unlock()
}

// PingCount is how many keepalive requests have arrived.
func (s *Server) PingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// Commands returns every command the fixture has been asked to run.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// ConnCount is how many connections the server has completed a handshake for.
func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// WaitForConnection blocks until the server has finished its side of a
// handshake. A client can consider itself connected a moment before the server
// records the connection, and dropping "every" connection in that window would
// drop none.
func (s *Server) WaitForConnection(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.ConnCount() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the fixture never registered an incoming connection")
}

// DropConnections kills every live session while leaving the listener up, which
// is what an sshd restart looks like from the client's side.
func (s *Server) DropConnections() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()

	for _, c := range conns {
		c.Close()
	}
}

// Close stops the server. It is registered as test cleanup by New.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()

	s.listener.Close()
	for _, c := range conns {
		c.Close()
	}
}

// Port is the loopback port the server listens on.
func (s *Server) Port() int { return s.listener.Addr().(*net.TCPAddr).Port }

// Addr is the host:port the server listens on.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// NetAddr is the listener's address, for host key callbacks.
func (s *Server) NetAddr() net.Addr { return s.listener.Addr() }

// KeyPath is the client private key on disk, under the test's $HOME.
func (s *Server) KeyPath() string { return s.keyPath }

// ClientKey is the private half of the only credential the server accepts.
func (s *Server) ClientKey() ed25519.PrivateKey { return s.clientKeyPriv }

// HostKey is the server's public host key.
func (s *Server) HostKey() ssh.PublicKey { return s.hostKey.PublicKey() }

// Trust records the given servers in ~/.ssh/known_hosts and clears
// SSH_AUTH_SOCK, so whether the developer happens to be running an ssh-agent
// cannot change which authentication method wins — that difference would
// otherwise show up as coverage drift between a laptop and CI.
func Trust(t *testing.T, servers ...*Server) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	WriteKnownHosts(t, servers...)
}

// WriteKnownHosts replaces ~/.ssh/known_hosts with entries for these servers.
func WriteKnownHosts(t *testing.T, servers ...*Server) string {
	t.Helper()
	var b strings.Builder
	for _, srv := range servers {
		b.WriteString(KnownHostsLine(srv.Addr(), srv.HostKey()))
	}
	return writeSSHFile(t, "known_hosts", []byte(b.String()))
}

// KnownHostsLine renders one host key as a known_hosts entry.
func KnownHostsLine(addr string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{addr}, key) + "\n"
}

// InstallDefaultKey writes a server's client key as ~/.ssh/id_ed25519, which is
// where target resolution looks when nothing names an IdentityFile — the
// situation a ProxyJump host lands in.
func InstallDefaultKey(t *testing.T, srv *Server) {
	t.Helper()
	src, err := os.ReadFile(srv.KeyPath())
	if err != nil {
		t.Fatalf("read fixture key: %v", err)
	}
	writeSSHFile(t, "id_ed25519", src)
}

// StartAgent runs an in-process ssh-agent holding the given keys and points
// SSH_AUTH_SOCK at it for the rest of the test.
func StartAgent(t *testing.T, keys ...ed25519.PrivateKey) {
	t.Helper()

	// The socket lives in a short temp path: unix socket names are capped near
	// 108 bytes, well under what a long test name would produce.
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.RemoveAll(dir)
	})

	keyring := agent.NewKeyring()
	for _, key := range keys {
		if err := keyring.Add(agent.AddedKey{PrivateKey: &key}); err != nil {
			t.Fatalf("add key to the agent: %v", err)
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, conn)
		}
	}()

	t.Setenv("SSH_AUTH_SOCK", sock)
}

// ClosedPort returns a loopback port with nothing listening on it.
func ClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// Key keeps a raw private key alongside its signer: writing a key file needs the
// former and configuring the server needs the latter.
type Key struct {
	Private ed25519.PrivateKey
	Signer  ssh.Signer
}

// GenerateKey creates a fresh ed25519 key pair.
func GenerateKey(t *testing.T) Key {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return Key{Private: priv, Signer: signer}
}

// MarshalPrivateKey renders a key in the OpenSSH PEM form, optionally encrypted.
func MarshalPrivateKey(t *testing.T, key ed25519.PrivateKey, passphrase string) []byte {
	t.Helper()
	var (
		block *pem.Block
		err   error
	)
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(key, "auto-tunnel test")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "auto-tunnel test", []byte(passphrase))
	}
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// SSHDir returns $HOME/.ssh, creating it. Callers are expected to have pointed
// HOME at a temp directory first.
func SSHDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	return dir
}

// writeClientKey puts the fixture's key in the test's own temp directory rather
// than under $HOME, so a package that only needs a client does not have to
// redirect HOME to keep the developer's ~/.ssh untouched.
func writeClientKey(t *testing.T, key Key) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, MarshalPrivateKey(t, key.Private, ""), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// Dial connects to the fixture with its own key, pinning its host key. It is the
// short path for tests that need a live *ssh.Client and nothing else — no
// known_hosts, no $HOME.
func Dial(t *testing.T, srv *Server) *ssh.Client {
	t.Helper()

	signer, err := ssh.ParsePrivateKey(MarshalPrivateKey(t, srv.ClientKey(), ""))
	if err != nil {
		t.Fatalf("parse the fixture key: %v", err)
	}
	client, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(srv.HostKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial the fixture: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func writeSSHFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(SSHDir(t), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() { os.Remove(path) })
	return path
}

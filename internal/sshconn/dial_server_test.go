package sshconn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/beyto1974/auto-tunnel/internal/sshtest"
)

// dialFixture connects to the fixture and closes the client when the test ends.
func dialFixture(t *testing.T, srv *sshtest.Server) *ssh.Client {
	t.Helper()
	client, err := Dial(fixtureTarget(srv), 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestDialAuthenticatesWithAnOnDiskKey(t *testing.T) {
	srv := newTrustedServer(t)

	client := dialFixture(t, srv)

	if got := client.User(); got != "tester" {
		t.Errorf("authenticated as %q, want %q", got, "tester")
	}
}

func TestDialUsesTheDefaultTimeoutWhenGivenNone(t *testing.T) {
	srv := newTrustedServer(t)

	client, err := Dial(fixtureTarget(srv), 0)
	if err != nil {
		t.Fatalf("Dial with a zero timeout: %v", err)
	}
	client.Close()
}

func TestDialRefusesAnUntrustedHostKey(t *testing.T) {
	// Unattended forwarding is the whole point of this tool, so a first-contact
	// host key has to be a hard stop rather than a silent trust-on-first-use.
	srv := newTrustedServer(t)

	// Keep the entry for this address but swap in a different server's key, so
	// the host is known and the key it offers is wrong.
	other := sshtest.New(t)
	path := filepath.Join(sshtest.SSHDir(t), "known_hosts")
	if err := os.WriteFile(path, []byte(sshtest.KnownHostsLine(srv.Addr(), other.HostKey())), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	_, err := Dial(fixtureTarget(srv), 5*time.Second)
	if err == nil {
		t.Fatal("Dial succeeded against an unknown host key, want a refusal")
	}
	if !strings.Contains(err.Error(), "host key verification failed") {
		t.Errorf("error = %v, want the verification failure", err)
	}
}

func TestDialReportsAHostMissingFromKnownHosts(t *testing.T) {
	srv := sshtest.New(t)
	t.Setenv("SSH_AUTH_SOCK", "")
	// A known_hosts that exists but has no entry for this host.
	other := sshtest.New(t)
	sshtest.WriteKnownHosts(t, other)

	_, err := Dial(fixtureTarget(srv), 5*time.Second)
	if err == nil {
		t.Fatal("Dial succeeded for an unrecorded host, want a refusal")
	}
	if !strings.Contains(err.Error(), "not in known_hosts") {
		t.Errorf("error = %v, want the unknown-host message", err)
	}
	if !strings.Contains(err.Error(), "ssh-keyscan") {
		t.Errorf("error = %v, want the recovery hint", err)
	}
}

func TestHostKeyCallbackNeedsAKnownHostsFile(t *testing.T) {
	// With no known_hosts at all there is nothing to verify against, and the
	// message has to say how to create one rather than just failing to dial.
	os.Remove(filepath.Join(sshtest.SSHDir(t), "known_hosts"))

	_, err := hostKeyCallback()
	if err == nil {
		t.Fatal("hostKeyCallback succeeded with no known_hosts, want an error")
	}
	if !strings.Contains(err.Error(), "connect once with `ssh` first") {
		t.Errorf("error = %v, want the setup hint", err)
	}
}

func TestHostKeyCallbackReadsKnownHosts2(t *testing.T) {
	srv := sshtest.New(t)
	os.Remove(filepath.Join(sshtest.SSHDir(t), "known_hosts"))

	path := filepath.Join(sshtest.SSHDir(t), "known_hosts2")
	if err := os.WriteFile(path, []byte(sshtest.KnownHostsLine(srv.Addr(), srv.HostKey())), 0o600); err != nil {
		t.Fatalf("write known_hosts2: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	callback, err := hostKeyCallback()
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	if err := callback(srv.Addr(), srv.NetAddr(), srv.HostKey()); err != nil {
		t.Errorf("callback rejected a key recorded in known_hosts2: %v", err)
	}
}

func TestHostKeyCallbackRejectsAnUnparseableKnownHosts(t *testing.T) {
	path := filepath.Join(sshtest.SSHDir(t), "known_hosts")
	if err := os.WriteFile(path, []byte("this is not a known_hosts line\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	if _, err := hostKeyCallback(); err == nil {
		t.Fatal("hostKeyCallback accepted a corrupt known_hosts, want an error")
	} else if !strings.Contains(err.Error(), "read known_hosts") {
		t.Errorf("error = %v, want it to name the file it could not read", err)
	}
}

func TestAuthMethodsNeedsSomeCredential(t *testing.T) {
	// Without this the dial fails deep inside the handshake with "unable to
	// authenticate", which reads like a server problem rather than a missing key.
	t.Setenv("SSH_AUTH_SOCK", "")

	_, err := authMethods(&Target{Alias: "prod", User: "deploy", Host: "10.0.0.5", Port: 22})

	if err == nil {
		t.Fatal("authMethods succeeded with no agent and no keys, want an error")
	}
	if !strings.Contains(err.Error(), "no usable SSH credentials") {
		t.Errorf("error = %v, want the missing-credentials message", err)
	}
}

func TestAuthMethodsSkipsUnreadableKeys(t *testing.T) {
	// ssh_config routinely names IdentityFiles that are absent or unreadable;
	// one of those must not stop the usable key beside it from being offered.
	srv := newTrustedServer(t)

	garbage := filepath.Join(sshtest.SSHDir(t), "id_garbage")
	if err := os.WriteFile(garbage, []byte("not a key\n"), 0o600); err != nil {
		t.Fatalf("write garbage key: %v", err)
	}
	t.Cleanup(func() { os.Remove(garbage) })

	target := fixtureTarget(srv)
	target.IdentityFiles = []string{garbage, filepath.Join(sshtest.SSHDir(t), "does-not-exist"), srv.KeyPath()}

	client, err := Dial(target, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial with a broken key ahead of a good one: %v", err)
	}
	client.Close()
}

func TestAuthMethodsPrefersTheAgent(t *testing.T) {
	// The agent is tried first so an encrypted on-disk key never prompts for a
	// passphrase that the agent could have answered.
	srv := sshtest.New(t)
	sshtest.WriteKnownHosts(t, srv)
	sshtest.StartAgent(t, srv.ClientKey())

	target := fixtureTarget(srv)
	target.IdentityFiles = nil // the agent is the only credential left

	client, err := Dial(target, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial using the agent: %v", err)
	}
	client.Close()
}

func TestAuthMethodsIgnoresAnEmptyAgent(t *testing.T) {
	// An agent with no keys loaded is worse than no agent: offering it as a
	// method wastes an authentication attempt against the server's limit.
	srv := sshtest.New(t)
	sshtest.WriteKnownHosts(t, srv)
	sshtest.StartAgent(t) // no keys

	if _, err := authMethods(&Target{Alias: "prod"}); err == nil {
		t.Fatal("authMethods accepted an empty agent, want the missing-credentials error")
	}

	// The on-disk key still has to win through.
	client, err := Dial(fixtureTarget(srv), 5*time.Second)
	if err != nil {
		t.Fatalf("Dial with an empty agent and a good key: %v", err)
	}
	client.Close()
}

func TestLoadPrivateKey(t *testing.T) {
	key := sshtest.GenerateKey(t)
	dir := t.TempDir()

	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, sshtest.MarshalPrivateKey(t, key.Private, ""), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := loadPrivateKey(good); err != nil {
		t.Errorf("loadPrivateKey on a valid key: %v", err)
	}

	if _, err := loadPrivateKey(filepath.Join(dir, "absent")); err == nil {
		t.Error("loadPrivateKey succeeded on a missing file, want an error")
	}

	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("-----BEGIN NOTHING-----\n"), 0o600); err != nil {
		t.Fatalf("write bad key: %v", err)
	}
	if _, err := loadPrivateKey(bad); err == nil {
		t.Error("loadPrivateKey succeeded on garbage, want an error")
	}
}

func TestLoadPrivateKeyCannotPromptWithoutATerminal(t *testing.T) {
	// Under CI, cron, or a pipe there is nobody to type a passphrase, and
	// blocking forever on a hidden prompt is worse than saying so.
	key := sshtest.GenerateKey(t)
	path := filepath.Join(t.TempDir(), "encrypted")
	if err := os.WriteFile(path, sshtest.MarshalPrivateKey(t, key.Private, "hunter2"), 0o600); err != nil {
		t.Fatalf("write encrypted key: %v", err)
	}

	_, err := loadPrivateKey(path)

	if err == nil {
		t.Fatal("loadPrivateKey succeeded on an encrypted key with no terminal, want an error")
	}
	if !strings.Contains(err.Error(), "ssh-agent") {
		t.Errorf("error = %v, want it to point at ssh-agent", err)
	}
}

func TestDialRejectsMultiHopProxyJump(t *testing.T) {
	srv := newTrustedServer(t)

	target := fixtureTarget(srv)
	target.ProxyJump = "bastion-a,bastion-b"

	_, err := Dial(target, 5*time.Second)
	if err == nil {
		t.Fatal("Dial accepted a multi-hop ProxyJump, want a refusal")
	}
	if !strings.Contains(err.Error(), "multi-hop ProxyJump") {
		t.Errorf("error = %v, want the multi-hop refusal", err)
	}
}

func TestDialTreatsProxyJumpNoneAsDirect(t *testing.T) {
	// ssh_config spells "do not use a jump host" as `ProxyJump none`, and
	// ssh_config.Get hands that string straight through.
	srv := newTrustedServer(t)

	target := fixtureTarget(srv)
	target.ProxyJump = "none"

	client, err := Dial(target, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial with ProxyJump none: %v", err)
	}
	client.Close()
}

func TestDialReportsAnUnreachableJumpHost(t *testing.T) {
	srv := newTrustedServer(t)

	target := fixtureTarget(srv)
	// A closed port on loopback: resolvable, but nothing is listening.
	target.ProxyJump = "tester@127.0.0.1:" + strconv.Itoa(sshtest.ClosedPort(t))
	sshtest.InstallDefaultKey(t, srv)

	_, err := Dial(target, 2*time.Second)

	if err == nil {
		t.Fatal("Dial succeeded through a dead jump host, want an error")
	}
	if !strings.Contains(err.Error(), "dial jump host") {
		t.Errorf("error = %v, want it to name the failed hop", err)
	}
}

func TestDialThroughAJumpHost(t *testing.T) {
	// The jump host carries a direct-tcpip channel to the real target, so both
	// servers have to be trusted and both handshakes have to succeed.
	jump := sshtest.New(t)
	target := sshtest.New(t)
	sshtest.Trust(t, jump, target)

	spec := fixtureTarget(target)
	spec.ProxyJump = "tester@127.0.0.1:" + strconv.Itoa(jump.Port())
	// Dial resolves the jump host through ResolveTarget, which finds no
	// IdentityFile in the temp HOME and falls back to the default key names.
	sshtest.InstallDefaultKey(t, jump)

	client, err := Dial(spec, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial through a jump host: %v", err)
	}
	defer client.Close()

	out, err := RunCommand(t.Context(), client, "echo hi")
	if err != nil {
		t.Fatalf("RunCommand over the jumped connection: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("stdout = %q, want the fixture's empty default", out)
	}
}

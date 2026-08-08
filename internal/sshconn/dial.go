// Package sshconn resolves SSH targets from ~/.ssh/config and maintains a
// persistent, self-healing SSH connection to a single remote host.
package sshconn

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// DefaultDialTimeout bounds a single TCP+handshake attempt.
const DefaultDialTimeout = 10 * time.Second

// defaultKeyNames are tried when neither the agent nor ssh_config supplies a key.
var defaultKeyNames = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

const (
	// defaultSSHPort is assumed when a target spec names no port.
	defaultSSHPort = 22
	// defaultSSHPortStr is the same port as known_hosts sees it. Entries for
	// port 22 are stored under the bare hostname; every other port is stored
	// as [host]:port, and the two forms never match each other.
	defaultSSHPortStr = "22"
)

// Target is a fully resolved SSH destination.
type Target struct {
	Spec          string // exactly what the user typed
	Alias         string // the host token, before ssh_config resolution
	User          string
	Host          string
	Port          int
	IdentityFiles []string
	ProxyJump     string
}

// Addr is the host:port to dial.
func (t *Target) Addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

func (t *Target) String() string { return t.User + "@" + t.Addr() }

// ResolveTarget turns a user-supplied spec ("myserver", "user@10.0.0.5:2222")
// into a Target, filling anything unspecified from ~/.ssh/config and then from
// SSH's own defaults.
func ResolveTarget(spec string) (*Target, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, errors.New("empty ssh target")
	}
	t := &Target{Spec: spec}

	rest := spec
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		t.User, rest = rest[:i], rest[i+1:]
	}

	// Port suffix. Bracketed IPv6 ("[::1]:2222") and bare IPv6 ("::1") both land here.
	switch {
	case strings.HasPrefix(rest, "["):
		end := strings.Index(rest, "]")
		if end < 0 {
			return nil, fmt.Errorf("malformed address %q: missing closing bracket", spec)
		}
		host := rest[1:end]
		tail := rest[end+1:]
		if strings.HasPrefix(tail, ":") {
			p, err := strconv.Atoi(tail[1:])
			if err != nil {
				return nil, fmt.Errorf("malformed port in %q: %w", spec, err)
			}
			t.Port = p
		}
		rest = host
	case strings.Count(rest, ":") == 1:
		host, portStr, _ := strings.Cut(rest, ":")
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("malformed port in %q: %w", spec, err)
		}
		t.Port, rest = p, host
	}

	t.Alias = rest
	if t.Alias == "" {
		return nil, fmt.Errorf("no host in %q", spec)
	}

	// ssh_config fills only what the spec left blank.
	t.Host = ssh_config.Get(t.Alias, "HostName")
	if t.Host == "" {
		t.Host = t.Alias
	}
	if t.User == "" {
		t.User = ssh_config.Get(t.Alias, "User")
	}
	if t.Port == 0 {
		if p, err := strconv.Atoi(ssh_config.Get(t.Alias, "Port")); err == nil {
			t.Port = p
		}
	}
	t.ProxyJump = ssh_config.Get(t.Alias, "ProxyJump")
	for _, f := range ssh_config.GetAll(t.Alias, "IdentityFile") {
		if p := expandTilde(f); p != "" && fileExists(p) {
			t.IdentityFiles = appendUnique(t.IdentityFiles, p)
		}
	}

	if t.User == "" {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("no user in %q and current user unknown: %w", spec, err)
		}
		t.User = u.Username
	}
	if t.Port == 0 {
		t.Port = defaultSSHPort
	}
	for _, name := range defaultKeyNames {
		if p := expandTilde("~/.ssh/" + name); fileExists(p) {
			t.IdentityFiles = appendUnique(t.IdentityFiles, p)
		}
	}
	return t, nil
}

// Dial opens one SSH connection to the target, hopping through ProxyJump when
// ~/.ssh/config specifies one. The caller owns the returned client.
func Dial(t *Target, timeout time.Duration) (*ssh.Client, error) {
	cfg, err := clientConfig(t, timeout)
	if err != nil {
		return nil, err
	}

	if t.ProxyJump == "" || t.ProxyJump == "none" {
		client, err := ssh.Dial("tcp", t.Addr(), cfg)
		if err != nil {
			return nil, fmt.Errorf("ssh dial %s: %w", t.Addr(), err)
		}
		return client, nil
	}

	// Single-hop ProxyJump: chained jumps ("a,b") are not supported.
	if strings.Contains(t.ProxyJump, ",") {
		return nil, fmt.Errorf("multi-hop ProxyJump %q is not supported", t.ProxyJump)
	}
	jumpTarget, err := ResolveTarget(t.ProxyJump)
	if err != nil {
		return nil, fmt.Errorf("resolve ProxyJump %q: %w", t.ProxyJump, err)
	}
	if jumpTarget.ProxyJump != "" {
		return nil, fmt.Errorf("nested ProxyJump via %q is not supported", t.ProxyJump)
	}
	jumpClient, err := Dial(jumpTarget, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial jump host %s: %w", jumpTarget, err)
	}
	hop, err := jumpClient.Dial("tcp", t.Addr())
	if err != nil {
		jumpClient.Close()
		return nil, fmt.Errorf("dial %s via %s: %w", t.Addr(), jumpTarget, err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(hop, t.Addr(), cfg)
	if err != nil {
		hop.Close()
		jumpClient.Close()
		return nil, fmt.Errorf("ssh handshake with %s via %s: %w", t.Addr(), jumpTarget, err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	// Tear the jump connection down once the tunneled session ends.
	go func() {
		client.Wait()
		jumpClient.Close()
	}()
	return client, nil
}

func clientConfig(t *Target, timeout time.Duration) (*ssh.ClientConfig, error) {
	hostKey, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	methods, err := authMethods(t)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	return &ssh.ClientConfig{
		User:            t.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	}, nil
}

// authMethods prefers ssh-agent, then falls back to on-disk keys. Disk keys are
// loaded lazily so an encrypted key only prompts for a passphrase if the agent
// could not authenticate first.
func authMethods(t *Target) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			if signers, err := ag.Signers(); err == nil && len(signers) > 0 {
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			} else {
				conn.Close()
			}
		}
	}

	if len(t.IdentityFiles) > 0 {
		files := t.IdentityFiles
		methods = append(methods, ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
			var signers []ssh.Signer
			for _, path := range files {
				s, err := loadPrivateKey(path)
				if err != nil {
					// One unreadable key must not sink the others.
					fmt.Fprintf(os.Stderr, "auto-tunnel: skipping key %s: %v\n", path, err)
					continue
				}
				signers = append(signers, s)
			}
			return signers, nil
		}))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable SSH credentials: ssh-agent is empty or unset and no key found for %s", t.Alias)
	}
	return methods, nil
}

func loadPrivateKey(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("key is passphrase-protected and no terminal is available to prompt; add it to ssh-agent")
	}
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", path)
	pass, readErr := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if readErr != nil {
		return nil, fmt.Errorf("read passphrase: %w", readErr)
	}
	return ssh.ParsePrivateKeyWithPassphrase(pem, pass)
}

// hostKeyCallback verifies against known_hosts. Unknown hosts are a hard error:
// the whole point of this tool is unattended forwarding, so silently trusting a
// new key would be a real MITM window.
func hostKeyCallback() (ssh.HostKeyCallback, error) {
	var files []string
	for _, name := range []string{"~/.ssh/known_hosts", "~/.ssh/known_hosts2"} {
		if p := expandTilde(name); fileExists(p) {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no ~/.ssh/known_hosts file; connect once with `ssh` first to record the host key")
	}
	base, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("read known_hosts: %w", err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := base(hostname, remote, key)
		if err == nil {
			return nil
		}
		host, port, splitErr := net.SplitHostPort(hostname)
		if splitErr != nil {
			host, port = hostname, ""
		}
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			return fmt.Errorf("host key for %s is not in known_hosts (%s %s)%s; if you trust it, run: %s",
				hostname, key.Type(), ssh.FingerprintSHA256(key),
				defaultPortNote(base, host, port, remote, key),
				keyscanHint(host, port))
		}
		return fmt.Errorf("host key verification failed for %s (offered %s %s): %w",
			hostname, key.Type(), ssh.FingerprintSHA256(key), err)
	}, nil
}

// keyscanHint is the ssh-keyscan command that would record this host. The -p is
// the part that is easy to miss: known_hosts entries are port-qualified, so a
// scan without it writes an entry under the bare hostname, which can never
// satisfy a lookup for [host]:port — following the hint changes nothing and the
// identical refusal comes back.
func keyscanHint(host, port string) string {
	if port == "" || port == defaultSSHPortStr {
		return fmt.Sprintf("ssh-keyscan -H %s >> ~/.ssh/known_hosts", host)
	}
	return fmt.Sprintf("ssh-keyscan -H -p %s %s >> ~/.ssh/known_hosts", port, host)
}

// defaultPortNote names the confusing case: this exact key is already trusted
// for the host on port 22, so the host looks perfectly known and the refusal
// looks like a bug. It returns "" whenever that is not what happened, including
// when the host is genuinely unknown or is on the default port already.
func defaultPortNote(base ssh.HostKeyCallback, host, port string, remote net.Addr, key ssh.PublicKey) string {
	if port == "" || port == defaultSSHPortStr {
		return ""
	}
	if base(net.JoinHostPort(host, defaultSSHPortStr), remote, key) != nil {
		return ""
	}
	return fmt.Sprintf("; this exact key is already trusted for %s on port %s, but known_hosts "+
		"entries are port-qualified, so port %s needs its own [%s]:%s entry",
		host, defaultSSHPortStr, port, host, port)
}

func expandTilde(path string) string {
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

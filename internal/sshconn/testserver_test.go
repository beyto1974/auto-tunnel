package sshconn

import (
	"testing"

	"github.com/beyto1974/auto-tunnel/internal/sshtest"
)

// The SSH fixture itself lives in internal/sshtest, because discovery and the
// engine need the same server. These helpers only add what is specific to this
// package: turning a fixture into the Target that Dial takes.

// fixtureTarget points at a fixture. It deliberately does not go through
// ResolveTarget: that consults ssh_config, which would make the result depend on
// whoever is running the suite.
func fixtureTarget(srv *sshtest.Server) *Target {
	return &Target{
		Spec:          srv.Addr(),
		Alias:         "127.0.0.1",
		User:          "tester",
		Host:          "127.0.0.1",
		Port:          srv.Port(),
		IdentityFiles: []string{srv.KeyPath()},
	}
}

// newTrustedServer starts a fixture and records its host key in known_hosts.
func newTrustedServer(t *testing.T) *sshtest.Server {
	t.Helper()
	srv := sshtest.New(t)
	sshtest.Trust(t, srv)
	return srv
}

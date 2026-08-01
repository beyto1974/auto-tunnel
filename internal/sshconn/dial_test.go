package sshconn

import (
	"fmt"
	"os"
	"os/user"
	"testing"
)

// TestMain points HOME at an empty directory for every test in the package.
// ResolveTarget consults ssh_config.Get, which parses the real ~/.ssh/config of
// whoever runs the suite, and expandTilde resolves against the same home. Without
// this, results — and coverage — depend on the developer's SSH setup, and a
// machine that happens to define a matching Host entry sees different behaviour
// from a clean CI runner. ssh_config caches the parsed file behind a sync.Once,
// so this has to happen before any test touches it.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "sshconn-home")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp home: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home) // os.Exit skips deferred calls
	os.Exit(code)
}

func TestAppendUnique(t *testing.T) {
	// ssh_config can name the same IdentityFile more than once, directly and
	// through a default, and each duplicate would otherwise become another
	// pointless key load at connect time.
	var got []string
	for _, v := range []string{"a", "b", "a", "c", "b"} {
		got = appendUnique(got, v)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("appendUnique produced %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("appendUnique produced %v, want %v", got, want)
		}
	}
}

func TestResolveTargetExplicitForms(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}

	// The alias is deliberately implausible so a real ~/.ssh/config entry
	// cannot change the expected result.
	const alias = "auto-tunnel-test-host-does-not-exist"

	tests := []struct {
		name     string
		spec     string
		wantUser string
		wantHost string
		wantPort int
	}{
		{
			name:     "user host and port",
			spec:     "deploy@10.0.0.5:2222",
			wantUser: "deploy",
			wantHost: "10.0.0.5",
			wantPort: 2222,
		},
		{
			name:     "user and host, default port",
			spec:     "deploy@10.0.0.5",
			wantUser: "deploy",
			wantHost: "10.0.0.5",
			wantPort: 22,
		},
		{
			name:     "bare host falls back to the current user",
			spec:     alias,
			wantUser: me.Username,
			wantHost: alias,
			wantPort: 22,
		},
		{
			name:     "bracketed ipv6 with port",
			spec:     "deploy@[fe80::1]:2200",
			wantUser: "deploy",
			wantHost: "fe80::1",
			wantPort: 2200,
		},
		{
			name:     "bare ipv6 is not mistaken for a port",
			spec:     "deploy@[fe80::1]",
			wantUser: "deploy",
			wantHost: "fe80::1",
			wantPort: 22,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTarget(tt.spec)
			if err != nil {
				t.Fatalf("ResolveTarget(%q): %v", tt.spec, err)
			}
			if got.User != tt.wantUser {
				t.Errorf("User = %q, want %q", got.User, tt.wantUser)
			}
			if got.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, tt.wantHost)
			}
			if got.Port != tt.wantPort {
				t.Errorf("Port = %d, want %d", got.Port, tt.wantPort)
			}
		})
	}
}

func TestResolveTargetRejectsBadInput(t *testing.T) {
	for _, spec := range []string{
		"",
		"   ",
		"deploy@host:notaport",
		"deploy@[fe80::1:2200", // unclosed bracket
		"deploy@",
	} {
		if got, err := ResolveTarget(spec); err == nil {
			t.Errorf("ResolveTarget(%q) = %+v, want an error", spec, got)
		}
	}
}

func TestTargetAddrQuotesIPv6(t *testing.T) {
	target, err := ResolveTarget("deploy@[fe80::1]:2200")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got, want := target.Addr(), "[fe80::1]:2200"; got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
}

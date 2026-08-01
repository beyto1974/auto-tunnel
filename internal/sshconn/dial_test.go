package sshconn

import (
	"os/user"
	"testing"
)

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

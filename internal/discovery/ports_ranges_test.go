package discovery

import (
	"strings"
	"testing"
)

// TestParsePortsRejectsBadRanges covers the entries that parse as the right
// shape but describe something we cannot act on. Each must degrade to a warning:
// one odd row from `docker ps` should never blind the poll to the other
// containers on the host.
func TestParsePortsRejectsBadRanges(t *testing.T) {
	tests := []struct {
		name        string
		column      string
		wantWarning string
	}{
		{
			name:        "descending container range",
			column:      "0.0.0.0:8000-8002->8002-8000/tcp",
			wantWarning: "descending range",
		},
		{
			name:        "descending exposed range",
			column:      "8002-8000/tcp",
			wantWarning: "descending range",
		},
		{
			name:        "host and container ranges of different sizes",
			column:      "0.0.0.0:8000-8002->9000-9001/tcp",
			wantWarning: "differ in size",
		},
		{
			name:        "host port too large for an int",
			column:      "0.0.0.0:99999999999999999999->80/tcp",
			wantWarning: "value out of range",
		},
		{
			name:        "container port too large for an int",
			column:      "0.0.0.0:8000->99999999999999999999/tcp",
			wantWarning: "value out of range",
		},
		{
			name:        "exposed port too large for an int",
			column:      "8000-99999999999999999999/tcp",
			wantWarning: "value out of range",
		},
		{
			name:        "range wider than the cap",
			column:      "0.0.0.0:8000-9000->8000-9000/tcp",
			wantWarning: "exceeds the 64-port limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, warnings := ParsePorts(tt.column)

			if len(specs) != 0 {
				t.Errorf("ParsePorts(%q) produced %d specs, want none", tt.column, len(specs))
			}
			if len(warnings) != 1 {
				t.Fatalf("ParsePorts(%q) produced warnings %v, want exactly one", tt.column, warnings)
			}
			if !strings.Contains(warnings[0], tt.wantWarning) {
				t.Errorf("warning = %q, want it to mention %q", warnings[0], tt.wantWarning)
			}
		})
	}
}

func TestParsePortsKeepsGoodEntriesAlongsideBadOnes(t *testing.T) {
	specs, warnings := ParsePorts("0.0.0.0:8080->80/tcp, 8002-8000/tcp, 0.0.0.0:5432->5432/tcp")

	if len(specs) != 2 {
		t.Errorf("got %d specs, want the two parseable ones: %+v", len(specs), specs)
	}
	if len(warnings) != 1 {
		t.Errorf("got warnings %v, want exactly one", warnings)
	}
}

package discovery

import "testing"

func TestParsePorts(t *testing.T) {
	tests := []struct {
		name     string
		column   string
		want     []PortSpec
		warnings int
	}{
		{
			name:   "empty column",
			column: "",
			want:   nil,
		},
		{
			name:   "ipv4 and ipv6 rows collapse",
			column: "0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp",
			want:   []PortSpec{{HostIP: "0.0.0.0", HostPort: 5432, ContainerPort: 5432, Proto: ProtoTCP}},
		},
		{
			name:   "loopback publish keeps its host ip",
			column: "127.0.0.1:9000->9000/tcp",
			want:   []PortSpec{{HostIP: "127.0.0.1", HostPort: 9000, ContainerPort: 9000, Proto: ProtoTCP}},
		},
		{
			name:   "ephemeral host port differs from container port",
			column: "0.0.0.0:32768->80/tcp",
			want:   []PortSpec{{HostIP: "0.0.0.0", HostPort: 32768, ContainerPort: 80, Proto: ProtoTCP}},
		},
		{
			name:   "exposed only",
			column: "8080/tcp",
			want:   []PortSpec{{ContainerPort: 8080, Proto: ProtoTCP}},
		},
		{
			name:   "published entry wins over exposed entry for the same port",
			column: "8080/tcp, 0.0.0.0:8080->8080/tcp",
			want:   []PortSpec{{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 8080, Proto: ProtoTCP}},
		},
		{
			name:   "udp is parsed, not dropped",
			column: "0.0.0.0:53->53/udp",
			want:   []PortSpec{{HostIP: "0.0.0.0", HostPort: 53, ContainerPort: 53, Proto: ProtoUDP}},
		},
		{
			name:   "same port number on tcp and udp stays distinct",
			column: "0.0.0.0:53->53/tcp, 0.0.0.0:53->53/udp",
			want: []PortSpec{
				{HostIP: "0.0.0.0", HostPort: 53, ContainerPort: 53, Proto: ProtoTCP},
				{HostIP: "0.0.0.0", HostPort: 53, ContainerPort: 53, Proto: ProtoUDP},
			},
		},
		{
			name:   "collapsed range expands pairwise",
			column: "0.0.0.0:8000-8002->8000-8002/tcp",
			want: []PortSpec{
				{HostIP: "0.0.0.0", HostPort: 8000, ContainerPort: 8000, Proto: ProtoTCP},
				{HostIP: "0.0.0.0", HostPort: 8001, ContainerPort: 8001, Proto: ProtoTCP},
				{HostIP: "0.0.0.0", HostPort: 8002, ContainerPort: 8002, Proto: ProtoTCP},
			},
		},
		{
			name:   "exposed range expands",
			column: "9000-9001/tcp",
			want: []PortSpec{
				{ContainerPort: 9000, Proto: ProtoTCP},
				{ContainerPort: 9001, Proto: ProtoTCP},
			},
		},
		{
			name:   "shifted range maps host to container pairwise",
			column: "0.0.0.0:18000-18001->8000-8001/tcp",
			want: []PortSpec{
				{HostIP: "0.0.0.0", HostPort: 18000, ContainerPort: 8000, Proto: ProtoTCP},
				{HostIP: "0.0.0.0", HostPort: 18001, ContainerPort: 8001, Proto: ProtoTCP},
			},
		},
		{
			name:   "multi port container",
			column: "0.0.0.0:8080->80/tcp, [::]:8080->80/tcp, 0.0.0.0:8443->443/tcp, [::]:8443->443/tcp",
			want: []PortSpec{
				{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Proto: ProtoTCP},
				{HostIP: "0.0.0.0", HostPort: 8443, ContainerPort: 443, Proto: ProtoTCP},
			},
		},
		{
			name:     "oversized range is rejected with a warning, not silently truncated",
			column:   "0.0.0.0:1000-2000->1000-2000/tcp",
			want:     nil,
			warnings: 1,
		},
		{
			name:     "garbage entry warns but the rest survives",
			column:   "nonsense, 0.0.0.0:5432->5432/tcp",
			want:     []PortSpec{{HostIP: "0.0.0.0", HostPort: 5432, ContainerPort: 5432, Proto: ProtoTCP}},
			warnings: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings := ParsePorts(tt.column)
			if len(warnings) != tt.warnings {
				t.Errorf("warnings = %d (%v), want %d", len(warnings), warnings, tt.warnings)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d specs %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("spec[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestParsePortsRealWorld uses a Ports column captured verbatim from a running
// Temporal container: exposed ranges, single exposed ports, and one published
// port listed twice (IPv4 + IPv6) all in the same string.
func TestParsePortsRealWorld(t *testing.T) {
	const column = "6933-6935/tcp, 6939/tcp, 7234-7235/tcp, 7239/tcp, " +
		"0.0.0.0:13131->7233/tcp, [::]:13131->7233/tcp"

	got, warnings := ParsePorts(column)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := []PortSpec{
		{ContainerPort: 6933, Proto: ProtoTCP},
		{ContainerPort: 6934, Proto: ProtoTCP},
		{ContainerPort: 6935, Proto: ProtoTCP},
		{ContainerPort: 6939, Proto: ProtoTCP},
		{HostIP: "0.0.0.0", HostPort: 13131, ContainerPort: 7233, Proto: ProtoTCP},
		{ContainerPort: 7234, Proto: ProtoTCP},
		{ContainerPort: 7235, Proto: ProtoTCP},
		{ContainerPort: 7239, Proto: ProtoTCP},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d specs %+v, want %d", len(got), got, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("spec[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPortMapTargetAndKey(t *testing.T) {
	published := PortMap{
		ContainerID: "abc123", ContainerPort: 80, HostPort: 8080,
		Proto: ProtoTCP, TargetHost: "127.0.0.1",
	}
	if got, want := published.Target(), "127.0.0.1:8080"; got != want {
		t.Errorf("Target() = %q, want %q", got, want)
	}
	if got, want := published.Key(), "abc123:80/tcp"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if !published.Forwardable() {
		t.Error("tcp port should be forwardable")
	}

	exposed := PortMap{
		ContainerID: "abc123", ContainerPort: 5000,
		Proto: ProtoTCP, TargetHost: "172.17.0.4",
	}
	if got, want := exposed.Target(), "172.17.0.4:5000"; got != want {
		t.Errorf("Target() = %q, want %q", got, want)
	}

	udp := PortMap{Proto: ProtoUDP}
	if udp.Forwardable() {
		t.Error("udp port must not be forwardable: ssh carries tcp only")
	}
}

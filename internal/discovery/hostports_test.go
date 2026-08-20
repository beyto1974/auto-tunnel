package discovery

import (
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/beyto1974/auto-tunnel/internal/sshtest"
)

// ssOutput is what `ss -Hltnp` prints: no header, one socket per line.
const ssOutput = `LISTEN 0      4096   127.0.0.1:5432       0.0.0.0:*    users:(("postgres",pid=812,fd=7))
LISTEN 0      511      0.0.0.0:80          0.0.0.0:*    users:(("nginx",pid=901,fd=6),("nginx",pid=902,fd=6))
LISTEN 0      511         [::]:80             [::]:*    users:(("nginx",pid=901,fd=8))
LISTEN 0      128      0.0.0.0:22          0.0.0.0:*
LISTEN 0      64    10.0.0.5:9100          0.0.0.0:*    users:(("node_exporter",pid=1200,fd=3))
`

// netstatOutput is the fallback layout, headers and all.
const netstatOutput = `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 127.0.0.1:5432          0.0.0.0:*               LISTEN      812/postgres
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN      -
tcp6       0      0 :::80                   :::*                    LISTEN      901/nginx
udp        0      0 0.0.0.0:514             0.0.0.0:*                           700/rsyslogd
`

func TestParseListenersReadsSSOutput(t *testing.T) {
	got, warnings := ParseListeners(ssOutput)

	want := []Listener{
		{Addr: "0.0.0.0", Port: 22},
		{Addr: "0.0.0.0", Port: 80, Proc: "nginx"},
		{Addr: "127.0.0.1", Port: 5432, Proc: "postgres"},
		{Addr: "10.0.0.5", Port: 9100, Proc: "node_exporter"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseListeners = %+v, want %+v", got, want)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestParseListenersReadsNetstatOutput(t *testing.T) {
	// The default command falls back to netstat, so the parser cannot know which
	// tool answered and has to read both.
	got, warnings := ParseListeners(netstatOutput)

	want := []Listener{
		{Addr: "0.0.0.0", Port: 22}, // "-" is not a process name: the user is not root
		{Addr: "::", Port: 80, Proc: "nginx"},
		{Addr: "127.0.0.1", Port: 5432, Proc: "postgres"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseListeners = %+v, want %+v", got, want)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestParseListenersCollapsesTheIPv4AndIPv6RowsOfOneService(t *testing.T) {
	// A service listening on both families is one tunnel, not two rows fighting
	// over the same local port.
	got, _ := ParseListeners(ssOutput)
	for _, l := range got {
		if l.Port == 80 && l.Proc != "nginx" {
			t.Errorf("port 80 = %+v, want the nginx row kept", l)
		}
	}
	count := 0
	for _, l := range got {
		if l.Port == 80 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("port 80 appears %d times, want once", count)
	}
}

func TestParseListenersTakesAProcessNameFromTheSecondRow(t *testing.T) {
	// ss prints the families in whichever order it likes, and only one of them
	// may carry the users:(...) column.
	got, _ := ParseListeners(
		"LISTEN 0 128 0.0.0.0:8080 0.0.0.0:*\n" +
			"LISTEN 0 128 [::]:8080 [::]:* users:((\"caddy\",pid=5,fd=3))\n")

	if len(got) != 1 || got[0].Proc != "caddy" {
		t.Errorf("ParseListeners = %+v, want one caddy row", got)
	}
}

func TestParseListenersWarnsAboutUnreadableRows(t *testing.T) {
	got, warnings := ParseListeners(
		"LISTEN 0 128 not-an-address 0.0.0.0:*\n" +
			"LISTEN 0 128 127.0.0.1:99999 0.0.0.0:*\n" +
			"LISTEN 0 128 127.0.0.1:5432 0.0.0.0:*\n")

	if len(got) != 1 || got[0].Port != 5432 {
		t.Errorf("ParseListeners = %+v, want the one readable row", got)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want one per unreadable row", warnings)
	}
}

func TestParseListenersIgnoresEverythingThatIsNotAListeningTCPSocket(t *testing.T) {
	// ESTABLISHED sockets, UDP rows, and headers all share the output.
	got, warnings := ParseListeners(
		"ESTAB 0 0 10.0.0.5:22 10.0.0.9:51234\n" +
			"UNCONN 0 0 0.0.0.0:514 0.0.0.0:*\n" +
			"Netid State Recv-Q Send-Q Local Address:Port Peer Address:Port\n")

	if len(got) != 0 {
		t.Errorf("ParseListeners = %+v, want nothing", got)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestDialHost(t *testing.T) {
	// A wildcard bind is reachable on loopback from the remote side; a specific
	// address may be the only one that answers, so it is kept as bound.
	for addr, want := range map[string]string{
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"*":         "127.0.0.1",
		"":          "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"10.0.0.5":  "10.0.0.5",
		"::1":       "::1",
	} {
		if got := dialHost(addr); got != want {
			t.Errorf("dialHost(%q) = %q, want %q", addr, got, want)
		}
	}
}

// hostFixture starts an SSH server answering the listen command with out.
func hostFixture(t *testing.T, opts HostOptions, out string) (*HostDiscoverer, *ssh.Client) {
	t.Helper()

	srv := sshtest.New(t)
	srv.SetExec(func(cmd string) sshtest.ExecResult {
		if !strings.Contains(cmd, "ss ") && !strings.Contains(cmd, "netstat") {
			return sshtest.ExecResult{Stderr: "unexpected command: " + cmd, Exit: 127}
		}
		return sshtest.ExecResult{Stdout: out}
	})

	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return NewHost(opts), sshtest.Dial(t, srv)
}

func TestHostDiscoverBuildsForwardableRows(t *testing.T) {
	d, client := hostFixture(t, HostOptions{}, ssOutput)

	result, err := d.Discover(t.Context(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	byPort := map[int]PortMap{}
	for _, pm := range result.Maps {
		byPort[pm.ContainerPort] = pm
	}
	pg, ok := byPort[5432]
	if !ok {
		t.Fatalf("maps = %+v, want the postgres socket", result.Maps)
	}
	if pg.Name != "postgres" || pg.Src() != SourceHost || pg.Target() != "127.0.0.1:5432" {
		t.Errorf("postgres row = %+v, want a named host row dialed on loopback", pg)
	}
	if !pg.Forwardable() || !pg.Published() || pg.Offered {
		t.Errorf("postgres row = %+v, want a forwardable published row that is not merely offered", pg)
	}
	// A socket bound to one address has to be dialed at that address: loopback
	// would not answer.
	if got := byPort[9100].Target(); got != "10.0.0.5:9100" {
		t.Errorf("node_exporter target = %q, want the bound address", got)
	}
	// Without privilege there is no process name, and the port still has to be
	// identifiable in the table.
	if got := byPort[22].Name; got != "port 22" {
		t.Errorf("unnamed socket = %q, want a name derived from the port", got)
	}
}

func TestHostDiscoverSkipsAndExcludes(t *testing.T) {
	d, client := hostFixture(t, HostOptions{
		SkipPorts: map[int]bool{22: true},
		Exclude:   regexp.MustCompile(`node_exporter|^80$`),
	}, ssOutput)

	result, err := d.Discover(t.Context(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	for _, pm := range result.Maps {
		switch pm.ContainerPort {
		case 22:
			t.Error("the ssh port we are tunnelling through was not skipped")
		case 80:
			t.Error("-host-exclude matching a port number did not drop the row")
		case 9100:
			t.Error("-host-exclude matching a process name did not drop the row")
		}
	}
	if len(result.Maps) != 1 {
		t.Errorf("maps = %+v, want only postgres left", result.Maps)
	}
}

func TestHostDiscoverMarksRowsOfferedInSelectMode(t *testing.T) {
	d, client := hostFixture(t, HostOptions{Offered: true}, ssOutput)

	result, err := d.Discover(t.Context(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	for _, pm := range result.Maps {
		if !pm.Offered {
			t.Errorf("row %+v is forwarded, want it listed for the user to pick", pm)
		}
	}
}

func TestHostDiscoverReportsAFailingCommand(t *testing.T) {
	srv := sshtest.New(t)
	srv.SetExec(func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stderr: "ss: command not found", Exit: 127}
	})
	d := NewHost(HostOptions{Log: slog.New(slog.DiscardHandler)})

	_, err := d.Discover(t.Context(), sshtest.Dial(t, srv))
	if err == nil {
		t.Fatal("Discover succeeded with no way to list sockets")
	}
	if !strings.Contains(err.Error(), "command not found") {
		t.Errorf("err = %v, want the remote reason", err)
	}
}

func TestParseHostMode(t *testing.T) {
	for _, in := range []string{"off", "select", "all"} {
		mode, err := ParseHostMode(in)
		if err != nil || string(mode) != in {
			t.Errorf("ParseHostMode(%q) = (%q, %v)", in, mode, err)
		}
	}
	if _, err := ParseHostMode("yes"); err == nil {
		t.Error("ParseHostMode accepted an unknown mode")
	}
	if !HostPortsSelect.Offered() || HostPortsAll.Offered() {
		t.Error("select must list rows without forwarding them, and all must forward them")
	}
	// The zero value has to behave like the flag default in both respects, or a
	// config assembled in code would quietly forward every service on the host.
	var zero HostMode
	if !zero.Enabled() || !zero.Offered() {
		t.Errorf("the zero HostMode polls=%v offers=%v, want it to behave like select", zero.Enabled(), zero.Offered())
	}
	if HostPortsOff.Enabled() {
		t.Error("off must not poll the remote host at all")
	}
}

func TestNewHostFillsInDefaults(t *testing.T) {
	d := NewHost(HostOptions{})
	if d.opts.Command != DefaultListenCommand {
		t.Errorf("Command = %q, want the default", d.opts.Command)
	}
	if d.opts.Log == nil {
		t.Error("Log is nil; every call site would panic")
	}
}

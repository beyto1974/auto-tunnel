package discovery

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/beyto1974/auto-tunnel/internal/sshtest"
)

// psLines renders one JSON object per line, the shape of
// `docker ps --format '{{json .}}'`.
func psLines(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

const (
	webID   = "0123456789ab"
	dbID    = "abcdef012345"
	cacheID = "fedcba543210"
)

// dockerFixture starts an SSH server whose "shell" answers ps and inspect from
// the strings given, and returns a Discoverer wired to it.
func dockerFixture(t *testing.T, opts Options, ps string, inspect string) (*Discoverer, *ssh.Client) {
	t.Helper()

	srv := sshtest.New(t)
	srv.SetExec(func(cmd string) sshtest.ExecResult {
		switch {
		case strings.HasPrefix(cmd, DefaultPSCommand):
			return sshtest.ExecResult{Stdout: ps}
		case strings.HasPrefix(cmd, DefaultInspectCommand):
			if inspect == "" {
				return sshtest.ExecResult{Stderr: "no such container", Exit: 1}
			}
			return sshtest.ExecResult{Stdout: inspect}
		default:
			return sshtest.ExecResult{Stderr: "unexpected command: " + cmd, Exit: 127}
		}
	})

	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return New(opts), sshtest.Dial(t, srv)
}

func discover(t *testing.T, d *Discoverer, client *ssh.Client) *Result {
	t.Helper()
	result, err := d.Discover(t.Context(), client)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return result
}

func TestNewFillsInDefaults(t *testing.T) {
	d := New(Options{})

	if d.opts.PSCommand != DefaultPSCommand {
		t.Errorf("PSCommand = %q, want the default", d.opts.PSCommand)
	}
	if d.opts.InspectCommand != DefaultInspectCommand {
		t.Errorf("InspectCommand = %q, want the default", d.opts.InspectCommand)
	}
	if d.opts.Log == nil {
		t.Error("Log is nil; every discovery path logs")
	}
}

func TestDiscoverParsesPublishedPorts(t *testing.T) {
	d, client := dockerFixture(t, Options{}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx:alpine","Ports":"0.0.0.0:8080->80/tcp","State":"running"}`,
		`{"ID":"`+dbID+`","Names":"db,db-primary","Image":"postgres:16","Ports":"0.0.0.0:5432->5432/tcp","State":"running"}`,
	), "")

	result := discover(t, d, client)

	if result.Containers != 2 {
		t.Errorf("Containers = %d, want 2", result.Containers)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", result.Warnings)
	}
	if len(result.Maps) != 2 {
		t.Fatalf("got %d port maps, want 2", len(result.Maps))
	}

	// Sorted by name, so db comes before web.
	if result.Maps[0].Name != "db" || result.Maps[1].Name != "web" {
		t.Errorf("maps ordered %q, %q; want db before web", result.Maps[0].Name, result.Maps[1].Name)
	}
	// A published port is reached through the remote host's loopback, which works
	// even for ports published only to 127.0.0.1.
	if got := result.Maps[0].TargetHost; got != loopback {
		t.Errorf("TargetHost = %q, want %q", got, loopback)
	}
	// Docker lists every alias; only the first names the container.
	if got := result.Maps[0].Name; got != "db" {
		t.Errorf("Name = %q, want the first of the comma-separated names", got)
	}
}

func TestDiscoverSortsPortsOfOneContainer(t *testing.T) {
	d, client := dockerFixture(t, Options{}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"0.0.0.0:8443->443/tcp, 0.0.0.0:8080->80/tcp","State":"running"}`,
	), "")

	result := discover(t, d, client)

	if len(result.Maps) != 2 {
		t.Fatalf("got %d port maps, want 2", len(result.Maps))
	}
	if result.Maps[0].ContainerPort != 80 || result.Maps[1].ContainerPort != 443 {
		t.Errorf("ports ordered %d, %d; want 80 before 443",
			result.Maps[0].ContainerPort, result.Maps[1].ContainerPort)
	}
}

func TestDiscoverSkipsUnpublishedPortsByDefault(t *testing.T) {
	// Forwarding an EXPOSEd-only port needs the container's IP, which costs an
	// extra remote command; it stays opt-in.
	d, client := dockerFixture(t, Options{}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"80/tcp","State":"running"}`,
	), "")

	result := discover(t, d, client)

	if result.Containers != 1 {
		t.Errorf("Containers = %d, want the container still counted", result.Containers)
	}
	if len(result.Maps) != 0 {
		t.Errorf("got %+v, want no port maps", result.Maps)
	}
}

func TestDiscoverResolvesUnpublishedPortsToContainerIPs(t *testing.T) {
	d, client := dockerFixture(t, Options{IncludeUnpublished: true}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"80/tcp","State":"running"}`,
	), webID+"cdef 172.17.0.4 \n")

	result := discover(t, d, client)

	if len(result.Maps) != 1 {
		t.Fatalf("got %+v, want one port map", result.Maps)
	}
	// docker ps reports short IDs and docker inspect reports full ones, so the
	// lookup has to match on prefix.
	if got := result.Maps[0].TargetHost; got != "172.17.0.4" {
		t.Errorf("TargetHost = %q, want the container IP", got)
	}
	if result.Maps[0].Published() {
		t.Error("the port is reported as published, want it marked unpublished")
	}
}

func TestDiscoverDropsUnpublishedPortsWithNoContainerIP(t *testing.T) {
	// Without an address there is nothing to dial, so the row would be a tunnel
	// that can never carry traffic.
	d, client := dockerFixture(t, Options{IncludeUnpublished: true}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"80/tcp","State":"running"}`,
	), "someotherid 172.17.0.9\n")

	result := discover(t, d, client)

	if len(result.Maps) != 0 {
		t.Errorf("got %+v, want the unresolvable port dropped", result.Maps)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "no container IP") {
		t.Errorf("warnings = %v, want one explaining the drop", result.Warnings)
	}
}

func TestDiscoverWarnsWhenInspectFails(t *testing.T) {
	d, client := dockerFixture(t, Options{IncludeUnpublished: true}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"80/tcp","State":"running"}`,
	), "") // an empty inspect script makes the command exit non-zero

	result := discover(t, d, client)

	if len(result.Maps) != 0 {
		t.Errorf("got %+v, want no maps when the IPs could not be read", result.Maps)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("no warning for a failed docker inspect")
	}
	if !strings.Contains(result.Warnings[0], "docker inspect") {
		t.Errorf("warnings = %v, want the inspect failure named", result.Warnings)
	}
}

func TestDiscoverRejectsContainerIDsThatAreNotDockerIDs(t *testing.T) {
	// The inspect command runs through the remote shell, and the README suggests
	// prefixing it with sudo: an ID carrying shell metacharacters would be remote
	// command execution rather than a failed lookup.
	d, client := dockerFixture(t, Options{IncludeUnpublished: true}, psLines(
		`{"ID":"abc; rm -rf /","Names":"evil","Image":"busybox","Ports":"80/tcp","State":"running"}`,
	), "")

	result := discover(t, d, client)

	if len(result.Warnings) == 0 {
		t.Fatal("no warning for a rejected container id")
	}
	if !strings.Contains(result.Warnings[0], "not a docker id") {
		t.Errorf("warnings = %v, want the id rejected by name", result.Warnings)
	}
	if len(result.Maps) != 0 {
		t.Errorf("got %+v, want the port dropped along with its id", result.Maps)
	}
}

func TestDiscoverWarnsOnAnUnparseableLine(t *testing.T) {
	// One malformed row must not blind the poll to the containers around it.
	d, client := dockerFixture(t, Options{}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"0.0.0.0:8080->80/tcp","State":"running"}`,
		`this is not json`,
		``,
		`{"ID":"`+dbID+`","Names":"db","Image":"postgres","Ports":"0.0.0.0:5432->5432/tcp","State":"running"}`,
	), "")

	result := discover(t, d, client)

	if result.Containers != 2 {
		t.Errorf("Containers = %d, want the two good rows", result.Containers)
	}
	if len(result.Maps) != 2 {
		t.Errorf("got %d maps, want 2", len(result.Maps))
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "unparseable") {
		t.Errorf("warnings = %v, want one about the bad line", result.Warnings)
	}
}

func TestDiscoverPrefixesPortWarningsWithTheContainerName(t *testing.T) {
	d, client := dockerFixture(t, Options{}, psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"nonsense","State":"running"}`,
	), "")

	result := discover(t, d, client)

	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", result.Warnings)
	}
	if !strings.HasPrefix(result.Warnings[0], "web: ") {
		t.Errorf("warning = %q, want it prefixed with the container name", result.Warnings[0])
	}
}

func TestDiscoverAppliesIncludeAndExclude(t *testing.T) {
	ps := psLines(
		`{"ID":"`+webID+`","Names":"web","Image":"nginx","Ports":"0.0.0.0:8080->80/tcp","State":"running"}`,
		`{"ID":"`+dbID+`","Names":"db","Image":"postgres","Ports":"0.0.0.0:5432->5432/tcp","State":"running"}`,
		`{"ID":"`+cacheID+`","Names":"web-cache","Image":"redis","Ports":"0.0.0.0:6379->6379/tcp","State":"running"}`,
	)

	tests := []struct {
		name  string
		opts  Options
		want  []string
		count int
	}{
		{
			name:  "no filters keeps everything",
			opts:  Options{},
			want:  []string{"db", "web", "web-cache"},
			count: 3,
		},
		{
			name:  "include narrows to a prefix",
			opts:  Options{Include: regexp.MustCompile(`^web`)},
			want:  []string{"web", "web-cache"},
			count: 2,
		},
		{
			name:  "exclude removes a match",
			opts:  Options{Exclude: regexp.MustCompile(`cache`)},
			want:  []string{"db", "web"},
			count: 2,
		},
		{
			name:  "exclude beats include",
			opts:  Options{Include: regexp.MustCompile(`^web`), Exclude: regexp.MustCompile(`cache`)},
			want:  []string{"web"},
			count: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := dockerFixture(t, tt.opts, ps, "")
			result := discover(t, d, client)

			if result.Containers != tt.count {
				t.Errorf("Containers = %d, want %d", result.Containers, tt.count)
			}
			var names []string
			for _, pm := range result.Maps {
				names = append(names, pm.Name)
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("kept %v, want %v", names, tt.want)
			}
		})
	}
}

func TestDiscoverFailsWhenDockerCannotBeReached(t *testing.T) {
	// A hiccup talking to Docker is reported so the UI can show it, and
	// deliberately does not invalidate the tunnels already running.
	srv := sshtest.New(t)
	srv.SetExec(func(string) sshtest.ExecResult {
		return sshtest.ExecResult{Stderr: "permission denied while trying to connect to the Docker daemon", Exit: 1}
	})
	d := New(Options{Log: slog.New(slog.DiscardHandler)})

	_, err := d.Discover(t.Context(), sshtest.Dial(t, srv))

	if err == nil {
		t.Fatal("Discover succeeded against a failing daemon, want an error")
	}
	if !strings.Contains(err.Error(), "docker discovery") {
		t.Errorf("error = %v, want it to name the step", err)
	}
}

func TestDiscoverStopsWithItsContext(t *testing.T) {
	srv := sshtest.New(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.SetExec(func(string) sshtest.ExecResult {
		<-release
		return sshtest.ExecResult{}
	})
	d := New(Options{Log: slog.New(slog.DiscardHandler)})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := d.Discover(ctx, sshtest.Dial(t, srv)); err == nil {
		t.Fatal("Discover ignored its cancelled context")
	}
}

func TestLookupByIDPrefix(t *testing.T) {
	ips := map[string]string{
		"0123456789abcdef": "172.17.0.2",
		"fedcba9876543210": "172.17.0.3",
	}

	if got := lookupByIDPrefix(ips, "0123456789abcdef"); got != "172.17.0.2" {
		t.Errorf("exact match = %q, want 172.17.0.2", got)
	}
	if got := lookupByIDPrefix(ips, "0123456789ab"); got != "172.17.0.2" {
		t.Errorf("prefix match = %q, want 172.17.0.2", got)
	}
	if got := lookupByIDPrefix(ips, "deadbeef"); got != "" {
		t.Errorf("miss = %q, want empty", got)
	}
}

func TestAttachContainerIPsLeavesPublishedPortsAlone(t *testing.T) {
	maps := []PortMap{
		{Name: "web", TargetHost: loopback, ContainerPort: 80},
		{Name: "db", ContainerID: "abc123", ContainerPort: 5432},
	}
	var warnings []string

	got := attachContainerIPs(maps, map[string]string{"abc123def": "172.17.0.5"}, &warnings)

	if len(got) != 2 {
		t.Fatalf("kept %d maps, want 2", len(got))
	}
	if got[0].TargetHost != loopback {
		t.Errorf("published TargetHost = %q, want it untouched", got[0].TargetHost)
	}
	if got[1].TargetHost != "172.17.0.5" {
		t.Errorf("unpublished TargetHost = %q, want the container IP", got[1].TargetHost)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

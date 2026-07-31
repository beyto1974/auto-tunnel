package discovery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/mustiko/auto-tunnel/internal/sshconn"
)

const (
	// DefaultPSCommand lists running containers, one JSON object per line.
	DefaultPSCommand = `docker ps --format '{{json .}}'`
	// DefaultInspectCommand is prefixed to a list of container IDs when
	// unpublished ports need a container IP to reach.
	DefaultInspectCommand = `docker inspect --format '{{.Id}} {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}'`

	// loopback is where a published port lives from the remote host's point of
	// view. Dialing it from the remote side works even for ports published only
	// to 127.0.0.1.
	loopback = "127.0.0.1"
)

// Options configures a Discoverer.
type Options struct {
	PSCommand          string
	InspectCommand     string
	Include            *regexp.Regexp // keep only containers whose name matches
	Exclude            *regexp.Regexp // drop containers whose name matches
	IncludeUnpublished bool           // also forward EXPOSEd-only ports via the container IP
	Log                *slog.Logger
}

// Discoverer polls a remote Docker daemon over an existing SSH connection.
type Discoverer struct {
	opts Options
}

// Result is one poll's worth of findings.
type Result struct {
	Maps       []PortMap
	Warnings   []string
	Containers int
}

// New creates a Discoverer, filling in default commands.
func New(opts Options) *Discoverer {
	if opts.PSCommand == "" {
		opts.PSCommand = DefaultPSCommand
	}
	if opts.InspectCommand == "" {
		opts.InspectCommand = DefaultInspectCommand
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Discoverer{opts: opts}
}

// psLine is the subset of `docker ps --format '{{json .}}'` we care about.
type psLine struct {
	ID    string `json:"ID"`
	Names string `json:"Names"`
	Image string `json:"Image"`
	Ports string `json:"Ports"`
	State string `json:"State"`
}

// Discover runs one poll. A failure to reach Docker is returned as an error so
// the caller can surface it; it deliberately does not invalidate tunnels that
// are already running.
func (d *Discoverer) Discover(ctx context.Context, client *ssh.Client) (*Result, error) {
	out, err := sshconn.RunCommand(ctx, client, d.opts.PSCommand)
	if err != nil {
		return nil, fmt.Errorf("docker discovery: %w", err)
	}

	result := &Result{}
	var needIP []string

	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c psLine
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("unparseable docker ps line: %v", err))
			continue
		}
		name := primaryName(c.Names)
		if !d.keep(name) {
			continue
		}
		result.Containers++

		specs, warnings := ParsePorts(c.Ports)
		for _, w := range warnings {
			result.Warnings = append(result.Warnings, name+": "+w)
		}
		for _, s := range specs {
			if !s.Published() && !d.opts.IncludeUnpublished {
				continue
			}
			pm := PortMap{
				ContainerID:   c.ID,
				Name:          name,
				Image:         c.Image,
				ContainerPort: s.ContainerPort,
				HostIP:        s.HostIP,
				HostPort:      s.HostPort,
				Proto:         s.Proto,
			}
			if s.Published() {
				pm.TargetHost = loopback
			} else {
				needIP = append(needIP, c.ID)
			}
			result.Maps = append(result.Maps, pm)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read docker ps output: %w", err)
	}

	if len(needIP) > 0 {
		ips, err := d.containerIPs(ctx, client, dedupe(needIP))
		if err != nil {
			result.Warnings = append(result.Warnings, err.Error())
		}
		result.Maps = attachContainerIPs(result.Maps, ips, &result.Warnings)
	}

	sort.Slice(result.Maps, func(i, j int) bool {
		if result.Maps[i].Name != result.Maps[j].Name {
			return result.Maps[i].Name < result.Maps[j].Name
		}
		return result.Maps[i].ContainerPort < result.Maps[j].ContainerPort
	})
	return result, nil
}

// keep applies the include/exclude name filters. Exclude wins over include.
func (d *Discoverer) keep(name string) bool {
	if d.opts.Exclude != nil && d.opts.Exclude.MatchString(name) {
		return false
	}
	if d.opts.Include != nil && !d.opts.Include.MatchString(name) {
		return false
	}
	return true
}

// containerIPs resolves the first network IP of each container, keyed by the
// full container ID.
func (d *Discoverer) containerIPs(ctx context.Context, client *ssh.Client, ids []string) (map[string]string, error) {
	cmd := d.opts.InspectCommand + " " + strings.Join(ids, " ")
	out, err := sshconn.RunCommand(ctx, client, cmd)
	if err != nil {
		return nil, fmt.Errorf("docker inspect for unpublished ports: %w", err)
	}
	ips := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ips[fields[0]] = fields[1] // first non-empty network address
	}
	return ips, nil
}

// attachContainerIPs fills TargetHost for unpublished ports and drops the ones
// whose container IP could not be resolved: without an address there is nothing
// to dial.
func attachContainerIPs(maps []PortMap, ips map[string]string, warnings *[]string) []PortMap {
	kept := maps[:0]
	for _, pm := range maps {
		if pm.TargetHost == "" {
			ip := lookupByIDPrefix(ips, pm.ContainerID)
			if ip == "" {
				*warnings = append(*warnings,
					fmt.Sprintf("%s: no container IP for unpublished port %d, skipping", pm.Name, pm.ContainerPort))
				continue
			}
			pm.TargetHost = ip
		}
		kept = append(kept, pm)
	}
	return kept
}

// lookupByIDPrefix matches the short IDs from `docker ps` against the full IDs
// from `docker inspect`.
func lookupByIDPrefix(ips map[string]string, id string) string {
	if ip, ok := ips[id]; ok {
		return ip
	}
	for full, ip := range ips {
		if strings.HasPrefix(full, id) {
			return ip
		}
	}
	return ""
}

// primaryName takes the first of Docker's comma-separated names.
func primaryName(names string) string {
	if i := strings.Index(names, ","); i >= 0 {
		return names[:i]
	}
	return names
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

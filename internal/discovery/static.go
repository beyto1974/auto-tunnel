package discovery

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseForward turns one -forward spec into forwarding candidates. The result
// does not depend on anything running remotely: these rows are published on
// every poll, so a declared forward survives a container exiting, a service
// restarting, and a failed discovery pass alike.
//
// The grammar is [label=]ports[@host], where ports is [local:]remote and either
// side may be a range:
//
//	5432                     local 5432    -> remote 127.0.0.1:5432
//	15432:5432               local 15432   -> remote 127.0.0.1:5432
//	8000-8010                ten ports, one for one
//	15432-15442:8000-8010    two ranges, which must be the same length
//	db=5432                  the row is named "db" instead of "forward 5432"
//	db=5432@10.0.0.5         dialed on the remote side at 10.0.0.5:5432
func ParseForward(spec string) ([]PortMap, error) {
	original := strings.TrimSpace(spec)
	rest := original
	if rest == "" {
		return nil, fmt.Errorf("empty forward spec")
	}

	var label string
	if i := strings.Index(rest, "="); i >= 0 {
		label, rest = strings.TrimSpace(rest[:i]), rest[i+1:]
		if label == "" {
			return nil, fmt.Errorf("forward %q: empty label", original)
		}
	}

	host := loopback
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		host, rest = strings.TrimSpace(rest[i+1:]), rest[:i]
		if host == "" {
			return nil, fmt.Errorf("forward %q: empty remote host", original)
		}
	}

	localSpec, remoteSpec := "", rest
	haveLocal := false
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		localSpec, remoteSpec, haveLocal = rest[:i], rest[i+1:], true
		if localSpec == "" {
			return nil, fmt.Errorf("forward %q: local port is missing before the colon", original)
		}
	}

	remote, err := expandPortSpec(remoteSpec)
	if err != nil {
		return nil, fmt.Errorf("forward %q: remote %v", original, err)
	}
	var local []int
	if haveLocal {
		local, err = expandPortSpec(localSpec)
		if err != nil {
			return nil, fmt.Errorf("forward %q: local %v", original, err)
		}
		if len(local) != len(remote) {
			return nil, fmt.Errorf("forward %q: local range holds %d ports, remote holds %d",
				original, len(local), len(remote))
		}
	}

	owner := label
	if owner == "" {
		owner = "forward"
	}

	maps := make([]PortMap, 0, len(remote))
	for i, rp := range remote {
		localPref := 0
		if local != nil {
			localPref = local[i]
		}
		maps = append(maps, PortMap{
			Source:        SourceStatic,
			Owner:         owner,
			Name:          forwardName(label, rp, len(remote)),
			Image:         original,
			ContainerPort: rp,
			// A declared forward is treated as published: there is a real
			// address on the remote side to dial, so no row should carry the
			// "unpublished, via container IP" note.
			HostIP:     host,
			HostPort:   rp,
			LocalPref:  localPref,
			Proto:      ProtoTCP,
			TargetHost: host,
		})
	}
	return maps, nil
}

// ParseForwards parses every spec, rejecting the whole set if one is bad: a
// typo in the third -forward should be a startup error, not a tunnel silently
// missing from the dashboard.
func ParseForwards(specs []string) ([]PortMap, error) {
	var maps []PortMap
	for _, spec := range specs {
		parsed, err := ParseForward(spec)
		if err != nil {
			return nil, err
		}
		maps = append(maps, parsed...)
	}
	return maps, nil
}

// forwardName keeps a labelled range distinguishable row by row, while a single
// labelled port reads as just its label.
func forwardName(label string, port, count int) string {
	switch {
	case label == "":
		return "forward " + strconv.Itoa(port)
	case count > 1:
		return label + ":" + strconv.Itoa(port)
	default:
		return label
	}
}

// expandPortSpec turns "5432" or "8000-8010" into concrete ports, reusing the
// range limit the Docker parser applies so one careless spec cannot open
// hundreds of local listeners.
func expandPortSpec(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("port is missing")
	}
	lo, hi := s, ""
	if i := strings.Index(s, "-"); i >= 0 {
		lo, hi = s[:i], s[i+1:]
		if hi == "" {
			return nil, fmt.Errorf("range %q is missing its upper bound", s)
		}
	}
	ports, err := expandRange(lo, hi)
	if err != nil {
		return nil, err
	}
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			return nil, fmt.Errorf("port %d is out of range", p)
		}
	}
	return ports, nil
}

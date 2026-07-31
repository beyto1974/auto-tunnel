package discovery

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxPortsPerRange caps expansion of a collapsed range such as
// "0.0.0.0:8000-8100->8000-8100/tcp". Without a cap, one careless `-p` on the
// remote would have us open hundreds of local listeners.
const MaxPortsPerRange = 64

var (
	// 0.0.0.0:5432->5432/tcp | [::]:8000-8002->8000-8002/tcp | 127.0.0.1:9000->9000/tcp
	publishedRe = regexp.MustCompile(`^(?:(\[[0-9a-fA-F:.%]+\]|[0-9][0-9.]*):)?(\d+)(?:-(\d+))?->(\d+)(?:-(\d+))?/([a-z]+)$`)
	// 8080/tcp | 8000-8002/tcp
	exposedRe = regexp.MustCompile(`^(\d+)(?:-(\d+))?/([a-z]+)$`)
)

// ParsePorts turns the Ports column of `docker ps` into port specs.
//
// The column looks like:
//
//	0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp, 8080/tcp
//
// Entries that describe the same container port more than once (typically the
// IPv4 and IPv6 rows of a single publish) collapse into one spec, preferring a
// published mapping over a merely exposed one. Unparseable entries are reported
// as warnings rather than failing the whole poll, since one odd row should not
// blind us to the rest of the containers.
func ParsePorts(column string) ([]PortSpec, []string) {
	var (
		specs    []PortSpec
		warnings []string
	)
	byPort := map[string]int{} // containerPort/proto -> index into specs

	add := func(s PortSpec) {
		key := strconv.Itoa(s.ContainerPort) + "/" + string(s.Proto)
		if i, seen := byPort[key]; seen {
			// Prefer a published mapping; among published ones, keep the first
			// (Docker lists IPv4 before IPv6).
			if !specs[i].Published() && s.Published() {
				specs[i] = s
			}
			return
		}
		byPort[key] = len(specs)
		specs = append(specs, s)
	}

	for _, raw := range strings.Split(column, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}

		if m := publishedRe.FindStringSubmatch(entry); m != nil {
			hostPorts, err := expandRange(m[2], m[3])
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("port %q: %v", entry, err))
				continue
			}
			containerPorts, err := expandRange(m[4], m[5])
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("port %q: %v", entry, err))
				continue
			}
			if len(hostPorts) != len(containerPorts) {
				warnings = append(warnings, fmt.Sprintf("port %q: host range and container range differ in size", entry))
				continue
			}
			hostIP := strings.Trim(m[1], "[]")
			for i := range hostPorts {
				add(PortSpec{
					HostIP:        hostIP,
					HostPort:      hostPorts[i],
					ContainerPort: containerPorts[i],
					Proto:         Proto(m[6]),
				})
			}
			continue
		}

		if m := exposedRe.FindStringSubmatch(entry); m != nil {
			containerPorts, err := expandRange(m[1], m[2])
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("port %q: %v", entry, err))
				continue
			}
			for _, cp := range containerPorts {
				add(PortSpec{ContainerPort: cp, Proto: Proto(m[3])})
			}
			continue
		}

		warnings = append(warnings, fmt.Sprintf("unrecognised port entry %q", entry))
	}

	sort.Slice(specs, func(i, j int) bool {
		if specs[i].ContainerPort != specs[j].ContainerPort {
			return specs[i].ContainerPort < specs[j].ContainerPort
		}
		return specs[i].Proto < specs[j].Proto
	})
	return specs, warnings
}

// expandRange turns "8000" or "8000".."8002" into the concrete ports.
func expandRange(loStr, hiStr string) ([]int, error) {
	lo, err := strconv.Atoi(loStr)
	if err != nil {
		return nil, err
	}
	if hiStr == "" {
		return []int{lo}, nil
	}
	hi, err := strconv.Atoi(hiStr)
	if err != nil {
		return nil, err
	}
	if hi < lo {
		return nil, fmt.Errorf("descending range %d-%d", lo, hi)
	}
	if hi-lo+1 > MaxPortsPerRange {
		return nil, fmt.Errorf("range %d-%d exceeds the %d-port limit", lo, hi, MaxPortsPerRange)
	}
	ports := make([]int, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		ports = append(ports, p)
	}
	return ports, nil
}

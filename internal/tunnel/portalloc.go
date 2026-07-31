// Package tunnel owns the local listeners that forward traffic to the remote
// host over SSH, and keeps the live set in sync with what discovery reports.
package tunnel

import (
	"fmt"
	"net"
	"strconv"
	"sync"
)

// DefaultFallbackBase is the first port tried when the preferred local port is
// already taken.
const DefaultFallbackBase = 20000

// DefaultFallbackSize is how many ports past the base are searched.
const DefaultFallbackSize = 1000

// Allocator hands out local listeners. Binding *is* the reservation: a port is
// claimed by successfully listening on it, so two tunnels can never race into
// the same port the way a "check then bind" scheme allows.
//
// Assignments are remembered per tunnel key, so a container that restarts comes
// back on the same local port even if its preferred port was taken by something
// else in the meantime.
type Allocator struct {
	bind         string
	fallbackBase int
	fallbackSize int

	mu       sync.Mutex
	assigned map[string]int
}

// NewAllocator creates an allocator binding on the given address.
func NewAllocator(bind string, fallbackBase, fallbackSize int) *Allocator {
	if bind == "" {
		bind = "127.0.0.1"
	}
	if fallbackBase <= 0 {
		fallbackBase = DefaultFallbackBase
	}
	if fallbackSize <= 0 {
		fallbackSize = DefaultFallbackSize
	}
	return &Allocator{
		bind:         bind,
		fallbackBase: fallbackBase,
		fallbackSize: fallbackSize,
		assigned:     map[string]int{},
	}
}

// Listen opens a listener for the given tunnel key, preferring (in order) the
// port this key used last, then the preferred port, then the fallback range.
func (a *Allocator) Listen(key string, preferred int) (net.Listener, int, error) {
	a.mu.Lock()
	sticky := a.assigned[key]
	a.mu.Unlock()

	var tried []int
	attempt := func(port int) (net.Listener, bool) {
		if port <= 0 || port > 65535 {
			return nil, false
		}
		for _, t := range tried {
			if t == port {
				return nil, false
			}
		}
		tried = append(tried, port)
		ln, err := net.Listen("tcp", net.JoinHostPort(a.bind, strconv.Itoa(port)))
		if err != nil {
			return nil, false
		}
		return ln, true
	}

	for _, port := range []int{sticky, preferred} {
		if ln, ok := attempt(port); ok {
			a.remember(key, port)
			return ln, port, nil
		}
	}
	for port := a.fallbackBase; port < a.fallbackBase+a.fallbackSize; port++ {
		if ln, ok := attempt(port); ok {
			a.remember(key, port)
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no free local port for %s: tried %d, then %d-%d",
		key, preferred, a.fallbackBase, a.fallbackBase+a.fallbackSize-1)
}

// Bind returns the address listeners are opened on.
func (a *Allocator) Bind() string { return a.bind }

func (a *Allocator) remember(key string, port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.assigned[key] = port
}

// Forget drops the remembered port for a key. Closing a listener alone keeps the
// assignment, which is what makes container restarts reclaim their old port.
func (a *Allocator) Forget(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.assigned, key)
}

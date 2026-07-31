package tunnel

import (
	"net"
	"strconv"
	"testing"
)

func TestAllocatorPrefersRequestedPort(t *testing.T) {
	a := NewAllocator("127.0.0.1", 20000, 100)

	preferred := freePort(t)
	ln, port, err := a.Listen("svc:80/tcp", preferred)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if port != preferred {
		t.Errorf("got port %d, want the preferred %d", port, preferred)
	}
}

func TestAllocatorFallsBackWhenPortIsTaken(t *testing.T) {
	taken := freePort(t)
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(taken)))
	if err != nil {
		t.Fatalf("could not occupy port %d: %v", taken, err)
	}
	defer blocker.Close()

	base := freePort(t)
	a := NewAllocator("127.0.0.1", base, 100)

	ln, port, err := a.Listen("svc:80/tcp", taken)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if port == taken {
		t.Fatalf("allocator handed out the occupied port %d", taken)
	}
	if port < base || port >= base+100 {
		t.Errorf("port %d is outside the fallback range %d-%d", port, base, base+99)
	}
}

func TestAllocatorReclaimsStickyPortAfterRestart(t *testing.T) {
	const key = "svc:80/tcp"
	base := freePort(t)
	a := NewAllocator("127.0.0.1", base, 100)

	preferred := freePort(t)
	first, port, err := a.Listen(key, preferred)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	first.Close()

	// The container restarts and now reports a different published port. The
	// tunnel should still come back on the local port callers already know.
	second, port2, err := a.Listen(key, preferred+1)
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer second.Close()

	if port2 != port {
		t.Errorf("got port %d after restart, want the sticky %d", port2, port)
	}

	a.Forget(key)
	third, port3, err := a.Listen(key, preferred+1)
	if err != nil {
		t.Fatalf("third Listen: %v", err)
	}
	defer third.Close()
	if port3 != preferred+1 {
		t.Errorf("after Forget got port %d, want the newly preferred %d", port3, preferred+1)
	}
}

func TestAllocatorExhaustedRangeFails(t *testing.T) {
	base := freePort(t)
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(base)))
	if err != nil {
		t.Fatalf("could not occupy port %d: %v", base, err)
	}
	defer blocker.Close()

	// Fallback range of exactly one port, and that port is taken.
	a := NewAllocator("127.0.0.1", base, 1)
	if _, _, err := a.Listen("svc:80/tcp", base); err == nil {
		t.Fatal("expected an error when every candidate port is occupied")
	}
}

// freePort returns a port that was free a moment ago. Tests use it as a
// plausible-but-unclaimed target; the allocator's contract is to bind or move
// on, never to assume.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

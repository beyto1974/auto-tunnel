package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func dialer() DialFunc { return net.Dial }

func addrOf(t *testing.T, rawURL string) string {
	t.Helper()
	// httptest URLs are http://127.0.0.1:port, and the probe wants host:port.
	for i := len(rawURL) - 1; i >= 0; i-- {
		if rawURL[i] == '/' {
			return rawURL[i+1:]
		}
	}
	t.Fatalf("cannot read an address out of %q", rawURL)
	return ""
}

func TestDetectPlaintextHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	if got := Detect(t.Context(), dialer(), addrOf(t, srv.URL), time.Second); got != SchemeHTTP {
		t.Errorf("Detect = %q, want http", got)
	}
}

func TestDetectHTTPS(t *testing.T) {
	// The certificate is self-signed, which is the normal case for a service
	// that was only ever meant to be reached over a tunnel.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	if got := Detect(t.Context(), dialer(), addrOf(t, srv.URL), 2*time.Second); got != SchemeHTTPS {
		t.Errorf("Detect = %q, want https", got)
	}
}

func TestDetectIgnoresSomethingThatIsNotHTTP(t *testing.T) {
	// A database or an SSH daemon answers with its own banner, or with nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("220 smtp ready\r\n"))
			c.Close()
		}
	}()

	if got := Detect(t.Context(), dialer(), ln.Addr().String(), time.Second); got != SchemeNone {
		t.Errorf("Detect = %q, want no scheme for a non-HTTP service", got)
	}
}

func TestDetectGivesUpOnASilentPort(t *testing.T) {
	// A port that accepts and then says nothing must not hang the prober: SSH
	// channels support no deadline, so this is the case the context has to
	// unblock by closing the connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var (
		mu       sync.Mutex
		accepted []net.Conn
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, c) // held open, never written to
			mu.Unlock()
		}
	}()

	start := time.Now()
	got := Detect(t.Context(), dialer(), ln.Addr().String(), 200*time.Millisecond)
	elapsed := time.Since(start)

	if got != SchemeNone {
		t.Errorf("Detect = %q, want no scheme", got)
	}
	// Two attempts, plaintext then TLS, so twice the timeout plus slack.
	if elapsed > 2*time.Second {
		t.Errorf("Detect took %s, want it bounded by the timeout", elapsed)
	}
}

func TestDetectSurvivesAPortThatIsNotThere(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening now

	if got := Detect(t.Context(), dialer(), addr, 500*time.Millisecond); got != SchemeNone {
		t.Errorf("Detect = %q, want no scheme when the dial fails", got)
	}
}

func TestDetectStopsWhenItsContextEnds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got := Detect(ctx, dialer(), ln.Addr().String(), time.Minute); got != SchemeNone {
		t.Errorf("Detect = %q, want no scheme once its context is done", got)
	}
}

package openurl

import (
	"runtime"
	"strings"
	"testing"
)

func TestOpenRefusesAnythingButHTTP(t *testing.T) {
	// The URL is built locally today, but this runs an external command: a
	// scheme somebody else chose is exactly what must never reach exec.
	for _, rawURL := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ssh://root@10.0.0.5",
		"://nonsense",
	} {
		if err := Open(rawURL); err == nil {
			t.Errorf("Open(%q) succeeded, want a refusal", rawURL)
		}
	}
}

func TestCommandPerPlatform(t *testing.T) {
	name, args := command("http://127.0.0.1:8080")
	if name == "" {
		t.Fatalf("no browser command for %s", runtime.GOOS)
	}
	if len(args) == 0 || !strings.Contains(strings.Join(args, " "), "http://127.0.0.1:8080") {
		t.Errorf("command(%s) = %q %v, want the url passed through", runtime.GOOS, name, args)
	}
	switch runtime.GOOS {
	case "darwin":
		if name != "open" {
			t.Errorf("name = %q, want open", name)
		}
	case "windows":
		if name != "rundll32" {
			t.Errorf("name = %q, want rundll32", name)
		}
	default:
		if name != "xdg-open" {
			t.Errorf("name = %q, want xdg-open", name)
		}
	}
}

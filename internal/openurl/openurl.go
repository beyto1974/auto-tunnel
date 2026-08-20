// Package openurl hands a forwarded service's URL to the desktop's browser. The
// dashboard prints URLs as plain text rather than as terminal hyperlinks, so
// this is what the "o" key uses to open one.
package openurl

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// Open asks the desktop to open rawURL in a browser.
//
// Only http and https are accepted. The URL is built from a probe result and a
// local port rather than from anything a remote host said, but this runs an
// external command, and a command built from a scheme somebody else chose is
// exactly the kind of thing worth refusing on principle.
func Open(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("not a url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("refusing to open a %q url", parsed.Scheme)
	}

	name, args := command(rawURL)
	if name == "" {
		return fmt.Errorf("no way to open a browser on %s", runtime.GOOS)
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// The browser outlives the dashboard, so the process is released rather
	// than waited for; nothing here cares about its exit status.
	go cmd.Wait()
	return nil
}

// command is what opens a URL on this platform.
func command(rawURL string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{rawURL}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return "xdg-open", []string{rawURL}
	}
}

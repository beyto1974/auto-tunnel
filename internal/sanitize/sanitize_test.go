package sanitize

import (
	"errors"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain container name", "example-api-1", "example-api-1"},
		{"utf8 passes through", "café-service", "café-service"},
		{"clear screen", "\x1b[2Jpwned", "�[2Jpwned"},
		{"osc 52 clipboard write", "x\x1b]52;c;cGF5bG9hZA==\x07", "x�]52;c;cGF5bG9hZA==�"},
		{"carriage return overwrite", "ok\rEVIL", "ok�EVIL"},
		{"newline splits a row", "a\nb", "a�b"},
		{"tab", "a\tb", "a�b"},
		{"del", "a\x7fb", "a�b"},
		{"c1 csi", "a2Jb", "a�2Jb"},
		{"invalid utf8", "a\xffb", "a�b"},
		{"real replacement char survives", "a�b", "a�b"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.in); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringLeavesNoEscape(t *testing.T) {
	got := String("\x1b[31mred\x1b[0m\x1b]0;title\x07")
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("String left an ESC byte in %q", got)
	}
}

func TestStrings(t *testing.T) {
	in := []string{"clean", "dir\x1bty"}
	got := Strings(in)
	if got[0] != "clean" || got[1] != "dir�ty" {
		t.Errorf("Strings() = %q", got)
	}
	if in[1] != "dir\x1bty" {
		t.Errorf("Strings mutated its input: %q", in)
	}
	if Strings(nil) != nil {
		t.Error("Strings(nil) should stay nil")
	}
}

func TestError(t *testing.T) {
	if got := Error(nil); got != "" {
		t.Errorf("Error(nil) = %q, want empty", got)
	}
	if got := Error(errors.New("boom\x1b[2J")); got != "boom�[2J" {
		t.Errorf("Error() = %q", got)
	}
}

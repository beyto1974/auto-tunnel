// Package sanitize strips terminal control sequences from strings that came
// from the remote host. Container names, image names, and remote stderr all end
// up on the user's terminal, and a hostile remote could otherwise emit escape
// sequences that rewrite the dashboard or drive the terminal emulator.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// replacement stands in for every control character that was removed. It is
// visible on purpose: silently dropping the bytes would hide the attempt.
const replacement = '�'

// String returns s with every control character replaced. C0 (including ESC,
// CR, LF, and TAB), DEL, and C1 all go, since any of them can start or end an
// escape sequence. Invalid UTF-8 is replaced too, so a byte string crafted to
// resolve into an escape after truncation cannot.
func String(s string) string {
	if isClean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch {
		case r == utf8.RuneError && !validRuneAt(s, i):
			b.WriteRune(replacement)
		case unicode.IsControl(r):
			b.WriteRune(replacement)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Strings sanitizes a slice, returning a new one so the caller's copy is
// untouched.
func Strings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = String(s)
	}
	return out
}

// Error sanitizes an error's message, returning "" for a nil error.
func Error(err error) string {
	if err == nil {
		return ""
	}
	return String(err.Error())
}

// isClean is the common case: nothing to rewrite, so nothing is allocated.
func isClean(s string) bool {
	for i, r := range s {
		if unicode.IsControl(r) {
			return false
		}
		if r == utf8.RuneError && !validRuneAt(s, i) {
			return false
		}
	}
	return true
}

// validRuneAt distinguishes a real U+FFFD in the input from the one range-over-
// string yields for an invalid byte.
func validRuneAt(s string, i int) bool {
	_, size := utf8.DecodeRuneInString(s[i:])
	return size == 3
}

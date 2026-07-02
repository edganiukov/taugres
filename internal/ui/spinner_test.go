package ui

import (
	"bytes"
	"strings"
	"testing"
)

// A non-terminal writer (bytes.Buffer) must produce clean, deterministic output
// with no ANSI escapes or animation frames.
func TestSpinnerNonTTYPlainOutput(t *testing.T) {
	var buf bytes.Buffer
	s := NewSpinner(&buf)
	if s.tty {
		t.Fatal("bytes.Buffer should not be detected as a terminal")
	}
	s.Start("installing node@22")
	s.Success("installed node@22")

	out := buf.String()
	if out != "installing node@22\n" {
		t.Errorf("unexpected non-tty output: %q", out)
	}
	if strings.Contains(out, "\033") || strings.Contains(out, "\r") {
		t.Errorf("non-tty output must not contain escapes: %q", out)
	}
}

func TestSpinnerNonTTYStopIsSilent(t *testing.T) {
	var buf bytes.Buffer
	s := NewSpinner(&buf)
	s.Start("working")
	s.Stop()
	if buf.String() != "working\n" {
		t.Errorf("Stop should not add output: %q", buf.String())
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		s       string
		maxCols int
		want    string
	}{
		{"hello", 10, "hello"}, // fits
		{"hello", 5, "hello"},  // exact
		{"hello", 4, "hel…"},   // cut with ellipsis
		{"hello", 1, "…"},      // single column
		{"hello", 0, ""},       // no room
		{"héllo", 3, "hé…"},    // rune-aware, not byte-aware
		{"", 5, ""},            // empty
	}
	for _, c := range cases {
		if got := truncate(c.s, c.maxCols); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.s, c.maxCols, got, c.want)
		}
	}
}

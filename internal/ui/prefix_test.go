package ui

import (
	"bytes"
	"testing"
)

func TestReporterStreamVerbosePrefixesLines(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(&buf, true) // verbose
	w := r.Stream("mise: ")
	w.Write([]byte("downloading\ninstalling"))
	r.Done() // flushes the trailing partial line

	want := "mise: downloading\nmise: installing\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestReporterStreamQuietOnNonTerminal(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(&buf, false) // default mode, non-terminal buffer
	r.Stream("mise: ").Write([]byte("noise\n"))
	r.Done()
	if buf.Len() != 0 {
		t.Errorf("expected quiet default mode on non-terminal, got %q", buf.String())
	}
}

func TestPrefixLines(t *testing.T) {
	got := PrefixLines("a\nb", "mise: ")
	if got != "mise: a\nmise: b" {
		t.Errorf("PrefixLines = %q", got)
	}
}

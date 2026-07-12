package ui

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestConfirm(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{" y \n", true},
		{"n\n", false},
		{"\n", false},
		{"", false}, // EOF
		{"nope\n", false},
	}
	for _, tc := range cases {
		var out strings.Builder
		got := Confirm(context.Background(), &out, strings.NewReader(tc.in), "install?")
		if got != tc.want {
			t.Errorf("Confirm(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if !strings.Contains(out.String(), "install? [y/N]: ") {
			t.Errorf("prompt not written for %q: %q", tc.in, out.String())
		}
	}
}

func TestConfirmNilReaderDeclines(t *testing.T) {
	var out strings.Builder
	if Confirm(context.Background(), &out, nil, "install?") {
		t.Error("nil reader should decline")
	}
}

func TestConfirmCancelledContextUnblocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A pipe with no writer blocks forever; only ctx cancellation can return.
	pr, pw := io.Pipe()
	defer pw.Close()

	var out strings.Builder
	if Confirm(ctx, &out, pr, "install?") {
		t.Error("cancelled context should decline")
	}
}

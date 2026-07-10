package toolenv

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestRunCancels verifies that cancelling the context stops a running child
// promptly (Ctrl+C during sync) instead of waiting for it to finish.
func TestRunCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := Run(ctx, exec.Command("sleep", "30"), nil, "", "sleep")
	if err == nil {
		t.Fatal("expected an error when the context is cancelled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run did not stop promptly on cancel: took %v", elapsed)
	}
}

// TestRunSuccess verifies the normal (uncancelled) path still returns nil.
func TestRunSuccess(t *testing.T) {
	if err := Run(context.Background(), exec.Command("true"), nil, "", "true"); err != nil {
		t.Errorf("Run(true) = %v, want nil", err)
	}
}

func TestScrapeVersions(t *testing.T) {
	out := []byte("Name: Foo_Bar\nVersion: 1.2.3\n---\nName: baz\nVersion: 4\n")
	versions := ScrapeVersions(out)
	if versions["foo-bar"] != "1.2.3" || versions["baz"] != "4" {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestTailBufferIsBounded(t *testing.T) {
	b := newTailBuffer(5)
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defg"))
	if got := b.String(); got != "cdefg" {
		t.Fatalf("tail = %q, want %q", got, "cdefg")
	}
	_, _ = b.Write([]byte("0123456789"))
	if got := b.String(); got != "56789" {
		t.Fatalf("large tail = %q, want %q", got, "56789")
	}
}

package trust

import (
	"os"
	"testing"

	"github.com/edganiukov/taugres/internal/testutil"
)

func TestAllowIsAllowedDeny(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := testutil.TempWorkspace(t)
	cfg := testutil.WriteFile(t, dir, "workspace.tg", "project(\"x\")\n")

	// Initially untrusted.
	if ok, err := IsAllowed(cfg); err != nil || ok {
		t.Fatalf("should be untrusted before allow (ok=%v err=%v)", ok, err)
	}

	if err := Allow(cfg); err != nil {
		t.Fatal(err)
	}
	if ok, _ := IsAllowed(cfg); !ok {
		t.Error("should be trusted after allow")
	}

	// Editing the config must NOT revoke trust: allow once is enough.
	if err := os.WriteFile(cfg, []byte("project(\"y\")\nshell.env(\"A\",\"b\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := IsAllowed(cfg); !ok {
		t.Error("edits should not revoke trust (allow once)")
	}

	// Deny removes trust.
	if err := Deny(cfg); err != nil {
		t.Fatal(err)
	}
	if ok, _ := IsAllowed(cfg); ok {
		t.Error("should be untrusted after deny")
	}
}

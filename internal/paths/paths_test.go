package paths

import (
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	repo := filepath.FromSlash("/home/user/repo")
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"root-anchored", "//node_modules/.bin", filepath.Join(repo, "node_modules", ".bin"), false},
		{"root-anchored extra slashes", "///scripts", filepath.Join(repo, "scripts"), false},
		{"bare root", "//", repo, false},
		{"absolute", "/opt/tool/bin", filepath.FromSlash("/opt/tool/bin"), false},
		{"dot-relative", "./foo", "", true},
		{"bare relative", "foo", "", true},
		{"parent relative", "../shared", "", true},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.input, repo)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNestedResolvesToRepoRoot(t *testing.T) {
	repo := filepath.FromSlash("/repo")
	got, err := Resolve("//taugres/lib/common.tg", repo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repo, "taugres", "lib", "common.tg")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIsRootAnchored(t *testing.T) {
	cases := map[string]bool{
		"//foo":  true,
		"/abs":   true,
		"foo":    false,
		"./foo":  false,
		"../foo": false,
	}
	for in, want := range cases {
		if got := IsRootAnchored(in); got != want {
			t.Errorf("IsRootAnchored(%q) = %v, want %v", in, got, want)
		}
	}
}

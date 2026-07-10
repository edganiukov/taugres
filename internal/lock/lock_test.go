package lock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsEmpty(t *testing.T) {
	f, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Mise) != 0 || len(f.Pip) != 0 || len(f.Npm) != 0 {
		t.Errorf("expected empty lock, got %+v", f)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := New()
	f.Mise["node"] = Entry{Requested: "22", Resolved: "22.11.0"}
	f.Pip["rich"] = Entry{Requested: "", Resolved: "13.9.4"}
	f.Section("cargo")["ripgrep"] = Entry{Requested: "", Resolved: "14.1.1"}
	if err := f.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("lockfile not written: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mise["node"] != (Entry{Requested: "22", Resolved: "22.11.0"}) {
		t.Errorf("mise entry = %+v", got.Mise["node"])
	}
	if got.Pip["rich"].Resolved != "13.9.4" {
		t.Errorf("pip entry = %+v", got.Pip["rich"])
	}
	if got.Section("cargo")["ripgrep"].Resolved != "14.1.1" {
		t.Errorf("generic manager entry = %+v", got.Section("cargo")["ripgrep"])
	}
}

func TestInstallVersion(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		entry   Entry
		present bool
		update  bool
		want    string
	}{
		{"new unpinned", "", Entry{}, false, false, ""},
		{"new pinned", "22", Entry{}, false, false, "22"},
		{"locked reproducible", "22", Entry{Requested: "22", Resolved: "22.11.0"}, true, false, "22.11.0"},
		{"locked unpinned reproducible", "", Entry{Requested: "", Resolved: "22.11.0"}, true, false, "22.11.0"},
		{"spec changed re-resolves", "23", Entry{Requested: "22", Resolved: "22.11.0"}, true, false, "23"},
		{"update unpinned -> latest", "", Entry{Requested: "", Resolved: "22.11.0"}, true, true, ""},
		{"update pinned stays locked", "22", Entry{Requested: "22", Resolved: "22.11.0"}, true, true, "22.11.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := InstallVersion(c.spec, c.entry, c.present, c.update); got != c.want {
				t.Errorf("InstallVersion(%q, %+v, %v, %v) = %q, want %q",
					c.spec, c.entry, c.present, c.update, got, c.want)
			}
		})
	}
}

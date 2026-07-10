package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/edganiukov/taugres/internal/model"
)

func BenchmarkNeedsSyncSteadyState(b *testing.B) {
	root := b.TempDir()
	stateDir := filepath.Join(root, ".taugres")
	inputs := make(map[string]string, 16)
	for i := range 16 {
		path := filepath.Join(root, fmt.Sprintf("input-%02d.tg", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
		inputs[path] = "hash"
	}
	toolDir := filepath.Join(stateDir, "tools", "bin")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		b.Fatal(err)
	}
	manifest := &Manifest{
		Inputs:   inputs,
		ToolDirs: []string{toolDir},
		Probes: []model.Probe{
			{Kind: "exists", Arg: root, Result: "1"},
			{Kind: "env", Arg: "TAU_BENCH_UNSET", Result: ""},
		},
	}
	if err := manifest.Write(stateDir); err != nil {
		b.Fatal(err)
	}
	configPath := filepath.Join(root, "input-00.tg")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		need, err := NeedsSync(stateDir, configPath)
		if err != nil || need {
			b.Fatalf("NeedsSync = %v, %v", need, err)
		}
	}
}

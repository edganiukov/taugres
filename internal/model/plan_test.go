package model

import "testing"

func TestResolvePlanDeepCopiesMutableState(t *testing.T) {
	evaluated := NewPlan()
	evaluated.EnvSet["A"] = "old"
	evaluated.PathPrepend = []string{"/one"}
	evaluated.DeferredEnv = []DeferredEnv{{Name: "D", Segments: []Segment{{Kind: SegLiteral, Value: "old"}}}}
	evaluated.SourceFuncs["f"] = []SourceFunc{{Name: "f", Shells: []string{"bash"}}}

	resolved := ResolvePlan(evaluated)
	resolved.EnvSet["A"] = "new"
	resolved.PathPrepend[0] = "/two"
	resolved.DeferredEnv[0].Segments[0].Value = "new"
	entry := resolved.SourceFuncs["f"][0]
	entry.Shells[0] = "zsh"
	resolved.SourceFuncs["f"][0] = entry

	if evaluated.EnvSet["A"] != "old" || evaluated.PathPrepend[0] != "/one" {
		t.Fatal("resolved plan mutated evaluated maps or slices")
	}
	if evaluated.DeferredEnv[0].Segments[0].Value != "old" {
		t.Fatal("resolved plan mutated evaluated deferred segments")
	}
	if evaluated.SourceFuncs["f"][0].Shells[0] != "bash" {
		t.Fatal("resolved plan mutated evaluated function shells")
	}
}

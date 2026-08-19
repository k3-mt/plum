package config

import "testing"

const sample = `
[repo]
languages    = ["go", "typescript"]   # trailing comment
test_command = "go test ./... -race"

[gating]
min_symbols_changed  = 3
new_public_surface   = false
divergence_threshold = 0.55

[conventions]
error_handling = "sentinel"
forbidden      = ["naked_return"]

[synthesis]
provider = "anthropic"
model    = "claude-sonnet-5"
`

func TestLoadAppliesOverridesAndKeepsDefaults(t *testing.T) {
	doc, err := ParseTOML(sample)
	if err != nil {
		t.Fatal(err)
	}
	c := Default("/repo")
	c.apply(doc)

	if len(c.Repo.Languages) != 2 || c.Repo.Languages[1] != "typescript" {
		t.Errorf("languages = %v", c.Repo.Languages)
	}
	if c.Repo.TestCommand != "go test ./... -race" {
		t.Errorf("test command = %q", c.Repo.TestCommand)
	}
	if c.Gating.MinSymbolsChanged != 3 || c.Gating.NewPublicSurface {
		t.Errorf("gating = %+v", c.Gating)
	}
	if c.Gating.DivergenceThreshold != 0.55 {
		t.Errorf("threshold = %v", c.Gating.DivergenceThreshold)
	}
	// Unset keys keep their defaults rather than becoming zero values.
	if !c.Gating.NewDependency || !c.Gating.MigrationTouched {
		t.Errorf("unset gating keys lost their defaults: %+v", c.Gating)
	}
	if c.Conventions.ErrorHandling != "sentinel" || !c.Forbids("naked_return") || c.Forbids("package_level_state") {
		t.Errorf("conventions = %+v", c.Conventions)
	}
	if c.Synthesis.Provider != "anthropic" {
		t.Errorf("provider = %q", c.Synthesis.Provider)
	}
}

func TestDefaultTOMLParses(t *testing.T) {
	if _, err := ParseTOML(DefaultTOML); err != nil {
		t.Fatalf("the config `plum init` writes must parse: %v", err)
	}
}

func TestCommentInsideStringSurvives(t *testing.T) {
	doc, err := ParseTOML(`[repo]` + "\n" + `test_command = "go test ./... # not a comment"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc["repo.test_command"].Str; got != "go test ./... # not a comment" {
		t.Errorf("got %q", got)
	}
}

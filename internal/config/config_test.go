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

// plum's own artifacts are committed by design and are JSON, so without an
// exclusion the config adapter reads its own bundles back in as if they were
// the project's configuration — thousands of "symbols" that are really plum
// looking at itself.
func TestPlumsOwnDirectoryIsExcludedWhateverTheConfigSays(t *testing.T) {
	c := Default("/repo")
	// An explicit list in an existing repo replaces the defaults, so relying on
	// a default entry would silently fail for every repo initialised earlier.
	c.Repo.Exclude = []string{"something/else/"}
	for _, path := range []string{
		".plum/sessions/2026-08-20-a1b2/bundle.json",
		".plum/sessions/2026-08-20-a1b2/landscape.json",
		".plum/config.toml",
		".plum",
	} {
		if !c.Excluded(path) {
			t.Errorf("%s must never be analysed, whatever the config says", path)
		}
	}
}

// A configured exclude list adds to the defaults. Replacing them meant every
// repo initialised before a new default existed silently never got it.
func TestConfiguredExclusionsAddToTheDefaults(t *testing.T) {
	doc, err := ParseTOML("[repo]\nexclude = [\"generated/\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	c := Default("/repo")
	c.apply(doc)
	if !c.Excluded("generated/thing.yaml") {
		t.Error("the configured exclusion was not applied")
	}
	for _, path := range []string{"node_modules/pkg/package.json", ".claude/settings.json", "vendor/x/config.yaml"} {
		if !c.Excluded(path) {
			t.Errorf("configuring an exclusion dropped the default for %s", path)
		}
	}
}

func TestDefaultExclusions(t *testing.T) {
	c := Default("/repo")
	for _, path := range []string{
		"testdata/fixtures/authcache/golden.json",
		"vendor/thing/config.yaml",
		".claude/settings.json",
		"node_modules/pkg/package.json",
	} {
		if !c.Excluded(path) {
			t.Errorf("%s should be excluded by default", path)
		}
	}
	for _, path := range []string{
		"config/app.yaml", "src/cache.js", "app/settings.json", "deploy/values.yaml",
	} {
		if c.Excluded(path) {
			t.Errorf("%s is the project's own file and must be analysed", path)
		}
	}
}

package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Dir is the per-repo footprint. Deliberately thin: the engine is installed,
// the repo holds only repo-truth (spec §3.2).
const Dir = ".plum"

type Config struct {
	Root string // absolute repo root

	Repo struct {
		Languages   []string
		TestCommand string
		JournalDir  string
		// Exclude holds path prefixes and globs that are code but not this
		// repo's own surface: fixtures, vendored trees, generated output.
		Exclude []string
	}
	Gating struct {
		MinSymbolsChanged   int
		NewPublicSurface    bool
		NewDependency       bool
		MigrationTouched    bool
		DivergenceThreshold float64
		RiskMarkers         int
	}
	Conventions struct {
		ErrorHandling string
		DIStyle       string
		Forbidden     []string
		Weights       map[string]float64
	}
	Synthesis struct {
		Provider string // anthropic | offline
		Model    string
		MaxDiff  int
	}
	Trace struct {
		Enabled     bool
		MaxEvents   int
		TestCommand string
		// Context decides how much of the surrounding system is recorded
		// alongside the change, for structure only:
		//   off   the changed symbols and nothing else
		//   file  everything else declared in the changed files (default)
		//   dir   everything declared beside them, same directory
		Context string
	}
	Auto struct {
		// Enabled gates automatic capture entirely.
		Enabled bool
		// OnGate names what runs when a session deserves attention. Capture is
		// always cheap; tracing and synthesis are not, and running them after
		// every prompt is how a tool stops being read (P6).
		OnGate []string
		// Notify surfaces a one-line message in the agent's UI when the gate fires.
		Notify bool
	}
	Ask struct {
		// Route is how a question from the explore UI gets answered:
		// "tmux" hands it to an agent session already running in a pane,
		// "api" calls the synthesis provider, "context-only" answers with the
		// assembled evidence and nothing else.
		Route      string
		TmuxTarget string
		TimeoutSec int
	}
}

func Default(root string) *Config {
	c := &Config{Root: root}
	c.Repo.Languages = []string{"go"}
	c.Repo.TestCommand = "go test ./..."
	c.Repo.JournalDir = filepath.Join(Dir, "journal")
	// .claude/ is agent-harness configuration, and plum's own `hooks install`
	// writes there — without this, installing plum shows up as a finding in
	// every session afterwards. Remove it from the list to audit hook changes.
	c.Repo.Exclude = []string{".claude/", "testdata/", "vendor/", "node_modules/", "dist/", ".git/"}
	c.Gating.MinSymbolsChanged = 5
	c.Gating.NewPublicSurface = true
	c.Gating.NewDependency = true
	c.Gating.MigrationTouched = true
	c.Gating.DivergenceThreshold = 0.4
	c.Gating.RiskMarkers = 3
	c.Conventions.ErrorHandling = "wrapped_error"
	c.Conventions.DIStyle = "constructor"
	c.Conventions.Forbidden = []string{"package_level_state", "naked_return", "init_side_effects"}
	c.Conventions.Weights = map[string]float64{"high": 1.0, "warn": 0.6, "info": 0.3}
	c.Synthesis.Provider = "offline"
	c.Synthesis.Model = "claude-sonnet-5"
	c.Synthesis.MaxDiff = 120000
	c.Trace.Enabled = true
	c.Trace.MaxEvents = 200000
	c.Trace.Context = "file"
	c.Auto.Enabled = true
	c.Auto.OnGate = []string{}
	c.Auto.Notify = true
	c.Ask.Route = "tmux"
	c.Ask.TimeoutSec = 300
	return c
}

// Load reads <root>/.plum/config.toml, falling back to defaults for absent keys.
func Load(root string) (*Config, error) {
	c := Default(root)
	data, err := os.ReadFile(filepath.Join(root, Dir, "config.toml"))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	doc, err := ParseTOML(string(data))
	if err != nil {
		return nil, err
	}
	c.apply(doc)
	return c, nil
}

func (c *Config) apply(d Doc) {
	str := func(k string, dst *string) {
		if v, ok := d[k]; ok && v.Kind == "string" {
			*dst = v.Str
		}
	}
	list := func(k string, dst *[]string) {
		if v, ok := d[k]; ok && v.Kind == "list" {
			*dst = v.List
		}
	}
	num := func(k string, dst *float64) {
		if v, ok := d[k]; ok && v.Kind == "number" {
			*dst = v.Num
		}
	}
	inum := func(k string, dst *int) {
		if v, ok := d[k]; ok && v.Kind == "number" {
			*dst = int(v.Num)
		}
	}
	bl := func(k string, dst *bool) {
		if v, ok := d[k]; ok && v.Kind == "bool" {
			*dst = v.Bool
		}
	}

	list("repo.languages", &c.Repo.Languages)
	str("repo.test_command", &c.Repo.TestCommand)
	str("repo.journal_dir", &c.Repo.JournalDir)
	// Exclusions are additive: a configured list adds to the defaults rather
	// than replacing them. Nobody sets `exclude` in order to *start* analysing
	// node_modules, and replacing meant a repo initialised before a new default
	// existed silently never got it.
	if v, ok := d["repo.exclude"]; ok && v.Kind == "list" {
		c.Repo.Exclude = append(c.Repo.Exclude, v.List...)
	}

	inum("gating.min_symbols_changed", &c.Gating.MinSymbolsChanged)
	bl("gating.new_public_surface", &c.Gating.NewPublicSurface)
	bl("gating.new_dependency", &c.Gating.NewDependency)
	bl("gating.migration_touched", &c.Gating.MigrationTouched)
	num("gating.divergence_threshold", &c.Gating.DivergenceThreshold)
	inum("gating.risk_markers", &c.Gating.RiskMarkers)

	str("conventions.error_handling", &c.Conventions.ErrorHandling)
	str("conventions.di_style", &c.Conventions.DIStyle)
	list("conventions.forbidden", &c.Conventions.Forbidden)
	for _, sev := range []string{"high", "warn", "info"} {
		if v, ok := d["conventions.weight_"+sev]; ok && v.Kind == "number" {
			c.Conventions.Weights[sev] = v.Num
		}
	}

	str("synthesis.provider", &c.Synthesis.Provider)
	str("synthesis.model", &c.Synthesis.Model)
	inum("synthesis.max_diff_bytes", &c.Synthesis.MaxDiff)

	bl("trace.enabled", &c.Trace.Enabled)
	inum("trace.max_events", &c.Trace.MaxEvents)
	str("trace.test_command", &c.Trace.TestCommand)
	str("trace.context", &c.Trace.Context)
	bl("auto.enabled", &c.Auto.Enabled)
	list("auto.on_gate", &c.Auto.OnGate)
	bl("auto.notify", &c.Auto.Notify)
	str("ask.route", &c.Ask.Route)
	str("ask.tmux_target", &c.Ask.TmuxTarget)
	inum("ask.timeout_seconds", &c.Ask.TimeoutSec)
	if c.Trace.TestCommand == "" {
		c.Trace.TestCommand = c.Repo.TestCommand
	}
}

func (c *Config) HasLanguage(name string) bool {
	for _, l := range c.Repo.Languages {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// Excluded reports whether a path is outside the audited surface. Matching is
// on path prefixes and simple globs — enough to name a fixtures directory
// without pulling in a glob library.
func (c *Config) Excluded(path string) bool {
	path = filepath.ToSlash(path)
	// plum's own directory is excluded unconditionally, not by configuration.
	// Its bundles and landscapes are JSON, so reading them back in would make
	// the tool analyse its own output — and an explicit `exclude` list in an
	// existing repo replaces the defaults, so a default alone would not hold.
	if path == Dir || strings.HasPrefix(path, Dir+"/") {
		return true
	}
	for _, pattern := range c.Repo.Exclude {
		pattern = filepath.ToSlash(pattern)
		switch {
		case strings.HasSuffix(pattern, "/"):
			if strings.HasPrefix(path, pattern) || strings.Contains(path, "/"+pattern) {
				return true
			}
		case strings.ContainsAny(pattern, "*?["):
			if ok, _ := filepath.Match(pattern, path); ok {
				return true
			}
			if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
				return true
			}
		default:
			if path == pattern || strings.HasPrefix(path, pattern+"/") {
				return true
			}
		}
	}
	return false
}

func (c *Config) Forbids(rule string) bool {
	for _, f := range c.Conventions.Forbidden {
		if f == rule {
			return true
		}
	}
	return false
}

// SessionsDir is where committed per-session artifacts live.
func (c *Config) SessionsDir() string { return filepath.Join(c.Root, Dir, "sessions") }

func (c *Config) SessionDir(id string) string { return filepath.Join(c.SessionsDir(), id) }

// DefaultTOML is written by `plum init`.
const DefaultTOML = `# PLUM — comprehension-debt system for agent-written code.
# See BUILD.md §6.7. Everything here is tunable; you will tune it.

[repo]
languages    = ["go"]
test_command = "go test ./..."
journal_dir  = ".plum/journal"
# Code that is not this repo's own surface: fixtures, vendored and generated trees.
# .claude/ holds agent-harness config, and plum hooks install writes there.
# Drop it from this list if you want changes to hooks surfaced as findings.
exclude      = [".claude/", "testdata/", "vendor/", "node_modules/", "dist/"]

[gating]
min_symbols_changed  = 5
new_public_surface   = true
new_dependency       = true
migration_touched    = true
divergence_threshold = 0.4
risk_markers         = 3

[conventions]
error_handling = "wrapped_error"   # wrapped_error | sentinel | panic
di_style       = "constructor"
forbidden      = ["package_level_state", "naked_return", "init_side_effects"]

[synthesis]
# "offline" composes the seams doc mechanically from the bundle (no network).
# "anthropic" calls the API with ANTHROPIC_API_KEY, in a fresh context (P2).
provider = "offline"
model    = "claude-sonnet-5"

[trace]
enabled    = true
max_events = 200000
# How much of the surrounding system is recorded alongside the change, for
# structure only: off | file | dir. A change is only legible inside the system
# it perturbs, but recording that system as deeply as the change costs more and
# says less.
context    = "file"

[auto]
# Automatic capture, for the Claude Code Stop hook and the git post-commit hook.
# Capture is milliseconds, so it always runs. Tracing and synthesis are not, and
# running them after every prompt is how a tool stops being read: name them here
# only if you want them on a session that fires the gate.
enabled  = true
on_gate  = []          # e.g. ["trace"] or ["trace", "synth"]
notify   = true

[ask]
# How a question asked in plum explore gets answered.
#   tmux         hand it to the agent session already running in a pane
#   api          call the synthesis provider directly (needs ANTHROPIC_API_KEY)
#   context-only answer with the assembled evidence and nothing else
route = "tmux"
# Leave empty to auto-detect the pane; set "session:window.pane" to pin it.
tmux_target     = ""
timeout_seconds = 300
`

// Detect works out what a repository is, so `plum init` fits the repo it runs in
// rather than assuming Go. It reports the languages present and a test command
// that suits the first of them. A repo with none of the markers falls back to
// Go — which is what plum itself is, and the safe default for the tool's own
// tree.
//
// Marker files are checked first because they are cheap and definitive; only a
// repo that carries none of them is scanned by file extension, and that scan is
// bounded so init stays fast on a large tree.
func Detect(root string) (languages []string, testCommand string) {
	has := func(names ...string) bool {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(root, n)); err == nil {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	add := func(lang string) {
		if !seen[lang] {
			seen[lang] = true
			languages = append(languages, lang)
		}
	}

	if has("go.mod") {
		add("go")
	}
	if has("pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile") {
		add("python")
	}
	if has("dbt_project.yml", "dbt_project.yaml") {
		add("dbt")
	}
	if has("tsconfig.json") {
		add("typescript")
	} else if has("package.json") {
		add("javascript")
	}

	// Nothing declared itself. Look at what files are actually here.
	if len(languages) == 0 {
		ext := scanExtensions(root)
		if ext[".go"] {
			add("go")
		}
		if ext[".py"] {
			add("python")
		}
		if ext[".ts"] || ext[".tsx"] {
			add("typescript")
		} else if ext[".js"] {
			add("javascript")
		}
	}
	if len(languages) == 0 {
		add("go")
	}
	return languages, testCommandFor(languages[0])
}

// testCommandFor is the command each kind of project is most often tested with.
// It is a starting point the config comment already tells the reader to tune,
// not a guarantee the command exists — a Python repo without pytest still gets
// the pytest line, because it is the right thing to change rather than to guess
// around.
func testCommandFor(lang string) string {
	switch lang {
	case "python":
		return "python3 -m pytest"
	case "typescript", "javascript":
		return "npm test"
	case "dbt":
		return "dbt build"
	default:
		return "go test ./..."
	}
}

// scanExtensions notes which source extensions appear in the tree, skipping the
// directories that never hold the repo's own surface and stopping after enough
// files to answer the question — init must not walk a monorepo end to end.
func scanExtensions(root string) map[string]bool {
	found := map[string]bool{}
	skip := map[string]bool{
		".git": true, ".plum": true, "vendor": true,
		"node_modules": true, "dist": true, "build": true,
	}
	const budget = 5000
	seen := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if seen++; seen > budget {
			return filepath.SkipAll
		}
		switch filepath.Ext(d.Name()) {
		case ".go":
			found[".go"] = true
		case ".py":
			found[".py"] = true
		case ".ts":
			found[".ts"] = true
		case ".tsx":
			found[".tsx"] = true
		case ".js":
			found[".js"] = true
		}
		return nil
	})
	return found
}

// InitTOML is the config `plum init` writes: the default template with the
// languages and test command set to what the repository actually is.
func InitTOML(root string) string {
	langs, cmd := Detect(root)
	s := DefaultTOML
	s = strings.Replace(s, `languages    = ["go"]`, "languages    = "+tomlList(langs), 1)
	s = strings.Replace(s, `test_command = "go test ./..."`, "test_command = "+strconv.Quote(cmd), 1)
	return s
}

func tomlList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// Package cli defines the commands. Stdlib flag only — the tool ships with zero
// third-party dependencies, which is what keeps CGO_ENABLED=0 trivially true (§13).
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/lang"
	"github.com/kelalaike/plum/internal/lang/conf"
	"github.com/kelalaike/plum/internal/lang/generic"
	"github.com/kelalaike/plum/internal/lang/gopkg"
	"github.com/kelalaike/plum/internal/lang/pyast"
	"github.com/kelalaike/plum/internal/store"
	"github.com/kelalaike/plum/internal/vcs"
)

var Version = "dev"

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env *Env, args []string) error
}

// Env is everything a command needs: repo root, config, git, adapters, store.
type Env struct {
	Cfg   *config.Config
	Repo  *vcs.Repo
	Store *store.Store
	Reg   *lang.Registry
	Out   *os.File
}

func commands() []command {
	return []command{
		{"init", "write .plum/config.toml and gitignore the machine-specific bits", cmdInit},
		{"run", "wrap an agent session and emit bundle.json", cmdRun},
		{"mark", "start|end — manual session boundaries for GUI editors", cmdMark},
		{"range", "extract a session from a commit range, with no wrapper at all", cmdRange},
		{"note", "record rationale live: what you chose and what you rejected", cmdNote},
		{"report", "bundle → markdown on stdout (default: latest session)", cmdReport},
		{"ls", "sessions with gate status", cmdLs},
		{"show", "raw bundle.json", cmdShow},
		{"synth", "synthesise the seams doc in a fresh context, extract claims", cmdSynth},
		{"stale", "re-fingerprint claims against the working tree", cmdStale},
		{"trace", "run the test suite under instrumentation, ingest events", cmdTrace},
		{"landscape", "derive the energy landscape from recorded traces", cmdLandscape},
		{"explore", "serve the landscape UI — no scoring, no gate, no timer", cmdExplore},
		{"ask", "ask a grounded question; the answer can be kept as rationale, a claim or a patch", cmdAsk},
		{"quiz", "interrogate yourself, graded against recorded execution", cmdQuiz},
		{"claims", "list|verify — executable claims", cmdClaims},
		{"gate", "exit non-zero when the session needs attention (for hooks)", cmdGate},
		{"version", "print the version", cmdVersion},
	}
}

func Main(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		return 0
	}
	name := args[0]
	for _, c := range commands() {
		if c.name != name {
			continue
		}
		env, err := newEnv(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "plum:", err)
			return 1
		}
		if err := c.run(ctx, env, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "plum:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "plum: unknown command %q\n\n", name)
	usage()
	return 2
}

func usage() {
	fmt.Println("plum — a comprehension-debt system for agent-written code.")
	fmt.Println()
	fmt.Println("The deliverable is not documentation. The deliverable is a correctly")
	fmt.Println("updated model in the developer's head.")
	fmt.Println()
	fmt.Println("usage: plum <command> [args]")
	fmt.Println()
	for _, c := range commands() {
		fmt.Printf("  %-10s %s\n", c.name, c.summary)
	}
	fmt.Println()
	fmt.Println("typical loop:")
	fmt.Println("  plum run -- claude      # wrap the session")
	fmt.Println("  plum report             # read the mechanical evidence")
	fmt.Println("  plum trace              # record real execution")
	fmt.Println("  plum explore            # meet the code as a landscape (P8)")
	fmt.Println("  plum quiz               # only now, and graded on traces (P4)")
}

func newEnv(ctx context.Context) (*Env, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	repo := vcs.New(wd)
	root, err := repo.Root(ctx)
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository: git is the substrate (P7)")
	}
	repo = vcs.New(root)
	cfg, err := config.Load(root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(config.Dir, "config.toml"), err)
	}
	return &Env{
		Cfg:   cfg,
		Repo:  repo,
		Store: store.New(cfg),
		Reg:   registry(cfg),
		Out:   os.Stdout,
	}, nil
}

// registry wires the adapters the config asks for. Go is native; the others use
// the line-based fallback until tree-sitter-under-wazero lands (§3.3).
func registry(cfg *config.Config) *lang.Registry {
	var as []lang.Adapter
	for _, l := range cfg.Repo.Languages {
		switch strings.ToLower(l) {
		case "go":
			as = append(as, gopkg.New())
		case "python", "py":
			// Python parses Python: exact signatures, docstrings and comments.
			// Falls back to the line-based adapter when no interpreter is found.
			if py := pyast.New(); py != nil {
				as = append(as, py)
			} else {
				as = append(as, generic.Python())
			}
		case "typescript", "javascript", "ts", "js":
			as = append(as, generic.TypeScript())
		}
	}
	if len(as) == 0 {
		as = append(as, gopkg.New())
	}
	// Configuration is always in scope. A changed default is a behaviour change
	// that no compiler and no test signature will announce.
	as = append(as, conf.New())
	return lang.NewRegistry(as...)
}

func cmdVersion(ctx context.Context, env *Env, args []string) error {
	fmt.Println("plum", Version)
	return nil
}

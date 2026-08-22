package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/capture"
	"github.com/k3-mt/plum/internal/config"
	"github.com/k3-mt/plum/internal/extract"
	"github.com/k3-mt/plum/internal/journal"
	"github.com/k3-mt/plum/internal/report"
	"github.com/k3-mt/plum/internal/stale"
	"github.com/k3-mt/plum/internal/trace"
)

func cmdInit(ctx context.Context, env *Env, args []string) error {
	dir := filepath.Join(env.Cfg.Root, config.Dir)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(env.Cfg.Root, env.Cfg.Repo.JournalDir), 0o755); err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// Fit the config to the repository rather than assume Go: a Python repo
		// gets `languages = ["python"]` and a pytest command, so `plum trace`
		// instruments something on the first run instead of reporting nothing.
		langs, cmd := config.Detect(env.Cfg.Root)
		if err := os.WriteFile(cfgPath, []byte(config.InitTOML(env.Cfg.Root)), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s — detected %s, test command %q\n", rel(env, cfgPath), strings.Join(langs, ", "), cmd)
	} else {
		fmt.Println("kept", rel(env, cfgPath), "(already present)")
	}

	// Traces are large and machine-specific; the journal is live rationale that
	// belongs to the run, not the repo. Everything else is committed on purpose.
	// Traces are large and machine-specific, the journal is live rationale, and a
	// question is a moment rather than a fact about the code. Bundles, synthesis,
	// claims and landscapes are committed on purpose.
	ignore := "sessions/*/traces/\ncurrent-session.json\nauto-state.json\njournal/\nask/\npatches/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(ignore), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", rel(env, filepath.Join(dir, ".gitignore")))
	fmt.Println()
	fmt.Println("next: plum run -- <your agent command>")
	return nil
}

func cmdRun(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "emit the bundle but not the report")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	argv := fs.Args()
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return fmt.Errorf("usage: plum run -- <command...>")
	}

	res, err := capture.Run(ctx, env.Cfg, env.Repo, argv)
	if err != nil {
		return err
	}
	b, err := extract.New(env.Repo, env.Cfg, env.Reg).Extract(ctx, res.Session, res.Journal)
	if err != nil {
		return err
	}
	if err := env.Store.Save(b); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "plum: session", b.Session.ID, "→", rel(env, env.Store.BundlePath(b.Session.ID)))
	if !*quiet {
		fmt.Fprintln(os.Stderr)
		out, err := renderReport(env, b, false)
		if err != nil {
			return err
		}
		fmt.Print(out)
	}
	if res.RunErr != nil {
		fmt.Fprintln(os.Stderr, "plum: wrapped command exited with error:", res.RunErr)
	}
	return nil
}

func cmdMark(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: plum mark start|end")
	}
	switch args[0] {
	case "start":
		agent := "manual"
		if len(args) > 1 {
			agent = args[1]
		}
		m, err := capture.MarkStart(ctx, env.Cfg, env.Repo, agent)
		if err != nil {
			return err
		}
		fmt.Printf("session open from %s at %s\n", m.StartSHA[:8], m.StartedAt.Format(time.RFC3339))
		return nil
	case "end":
		m, err := capture.LoadMark(env.Cfg)
		if err != nil {
			return fmt.Errorf("no open session — run `plum mark start` first")
		}
		sess, err := capture.Close(ctx, env.Cfg, env.Repo, m.StartSHA, m.StartedAt, m.Command, m.Agent)
		if err != nil {
			return err
		}
		j, _ := journal.Harvest(filepath.Join(env.Cfg.Root, env.Cfg.Repo.JournalDir), m.StartedAt)
		sess.JournalPresent = len(j) > 0
		b, err := extract.New(env.Repo, env.Cfg, env.Reg).Extract(ctx, *sess, j)
		if err != nil {
			return err
		}
		if err := env.Store.Save(b); err != nil {
			return err
		}
		if err := capture.ClearMark(env.Cfg); err != nil {
			return err
		}
		fmt.Println("session", b.Session.ID, "→", rel(env, env.Store.BundlePath(b.Session.ID)))
		return nil
	}
	return fmt.Errorf("usage: plum mark start|end")
}

// cmdRange is P7 taken literally: the pipeline depends on a commit range, not on
// any agent, editor or hook. It is also how you audit work that was already done.
func cmdRange(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("range", flag.ContinueOnError)
	agent := fs.String("agent", "unknown", "which tool produced the change")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	spec := first(fs.Args())
	if spec == "" {
		return fmt.Errorf("usage: plum range <from>..<to>   (to defaults to the working tree)")
	}
	from, to, _ := strings.Cut(spec, "..")
	startSHA, err := env.Repo.RevParse(ctx, from)
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	var sess *bundle.Session
	if to == "" {
		// No end given: close the range the way a wrapped session closes, which
		// includes uncommitted work via a dangling stash commit.
		sess, err = capture.Close(ctx, env.Cfg, env.Repo, startSHA, started, "range "+spec, *agent)
		if err != nil {
			return err
		}
	} else {
		endSHA, err := env.Repo.RevParse(ctx, to)
		if err != nil {
			return err
		}
		sess = &bundle.Session{
			ID: capture.SessionID(started, startSHA), StartSHA: startSHA, EndSHA: endSHA,
			StartedAt: started, EndedAt: started, Command: "range " + spec, Agent: *agent, Repo: env.Cfg.Root,
		}
	}
	j, _ := journal.Harvest(filepath.Join(env.Cfg.Root, env.Cfg.Repo.JournalDir), time.Time{})
	sess.JournalPresent = len(j) > 0
	b, err := extract.New(env.Repo, env.Cfg, env.Reg).Extract(ctx, *sess, j)
	if err != nil {
		return err
	}
	if err := env.Store.Save(b); err != nil {
		return err
	}
	fmt.Println("session", b.Session.ID, "→", rel(env, env.Store.BundlePath(b.Session.ID)))
	return nil
}

func cmdNote(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	file := fs.String("file", "", "the file this decision is about")
	tool := fs.String("tool", "human", "what produced the change")
	var alts multiFlag
	fs.Var(&alts, "rejected", "an alternative that was considered and rejected (repeatable)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	rationale := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if rationale == "" {
		return fmt.Errorf(`usage: plum note [-file f] [-rejected "..."] <why>`)
	}
	e := bundle.JournalEntry{
		TS: time.Now().UTC(), Tool: *tool, File: *file,
		Rationale: rationale, Alternatives: alts,
	}
	if err := journal.Append(filepath.Join(env.Cfg.Root, env.Cfg.Repo.JournalDir), e); err != nil {
		return err
	}
	fmt.Println("recorded")
	return nil
}

func cmdReport(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "include signatures")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(fs.Args()))
	if err != nil {
		return err
	}
	b, err := env.Store.Load(id)
	if err != nil {
		return err
	}
	out, err := renderReport(env, b, *verbose)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// renderReport re-fingerprints claims on every report. Without this, drift sets
// in within two weeks, you stop trusting the docs, and the system is dead (§8.3).
func renderReport(env *Env, b *bundle.Bundle, verbose bool) (string, error) {
	opt := report.Options{Verbose: verbose}
	id := b.Session.ID
	if cs, err := stale.Check(env.Cfg, env.Reg, env.Store.ClaimsPath(id)); err == nil {
		for _, c := range cs {
			opt.Stale = append(opt.Stale, report.StaleClaim{ID: c.ID, Claim: c.Claim, Symbol: c.Symbol, Reason: c.Reason})
		}
	}
	if l, err := loadLandscape(env, id); err == nil {
		opt.LandscapeNotes = l.Notes()
		opt.UnannotatedBarriers = l.UnannotatedExpensive()
	}
	// Recorded execution beats the name-matching heuristic wherever it exists.
	if events, err := trace.ReadFile(env.Store.TracePath(id)); err == nil && len(events) > 0 {
		opt.Reached = trace.Reached(events)
		opt.Traced = true
	}
	return report.Render(b, opt), nil
}

func cmdLs(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	latest := fs.Bool("latest", false, "print only the most recent session id")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	ids, err := env.Store.List()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("no sessions yet — plum run -- <command>")
		return nil
	}
	if *latest {
		fmt.Println(ids[len(ids)-1])
		return nil
	}
	fmt.Printf("%-20s %-6s %-8s %-8s %s\n", "SESSION", "GATE", "SYMBOLS", "SURFACE", "REASONS")
	for _, id := range ids {
		b, err := env.Store.Load(id)
		if err != nil {
			fmt.Printf("%-20s %-6s %s\n", id, "?", err)
			continue
		}
		gate := "clear"
		if b.Gate.Fired {
			gate = "FIRED"
		}
		surface := len(b.Surface.Added) + len(b.Surface.Removed) + len(b.Surface.Modified)
		fmt.Printf("%-20s %-6s %-8d %-8d %s\n", id, gate, len(b.Symbols), surface, strings.Join(b.Gate.Reasons, " · "))
	}
	return nil
}

func cmdShow(ctx context.Context, env *Env, args []string) error {
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(args))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(env.Store.BundlePath(id))
	if err != nil {
		return err
	}
	var pretty any
	if json.Unmarshal(data, &pretty) == nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(pretty)
	}
	_, err = os.Stdout.Write(data)
	return err
}

// cmdGate is the hook surface: exit 1 when this session deserves attention, so
// a git hook or a tmux popup can decide whether to interrupt (P6).
func cmdGate(ctx context.Context, env *Env, args []string) error {
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(args))
	if err != nil {
		return err
	}
	b, err := env.Store.Load(id)
	if err != nil {
		return err
	}
	if !b.Gate.Fired {
		fmt.Println("gate clear")
		return nil
	}
	fmt.Println("GATE FIRED —", strings.Join(b.Gate.Reasons, " · "))
	os.Exit(1)
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func rel(env *Env, path string) string {
	if r, err := filepath.Rel(env.Cfg.Root, path); err == nil {
		return r
	}
	return path
}

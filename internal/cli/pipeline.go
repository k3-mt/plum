package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/ask"
	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/explore"
	"github.com/kelalaike/plum/internal/interpret"
	"github.com/kelalaike/plum/internal/quiz"
	"github.com/kelalaike/plum/internal/server"
	"github.com/kelalaike/plum/internal/stale"
	"github.com/kelalaike/plum/internal/store"
	"github.com/kelalaike/plum/internal/synth"
	"github.com/kelalaike/plum/internal/trace"
)

// cmdSynth runs synthesis in a fresh process with no inherited context (P2).
func cmdSynth(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("synth", flag.ContinueOnError)
	provider := fs.String("provider", "", "override synthesis.provider (anthropic|offline)")
	print := fs.Bool("print", false, "write to stdout instead of the session dir")
	if err := fs.Parse(args); err != nil {
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
	name := env.Cfg.Synthesis.Provider
	if *provider != "" {
		name = *provider
	}
	p, err := providerFor(name, env, b)
	if err != nil {
		return err
	}
	diff, _ := env.Repo.Diff(ctx, b.Session.StartSHA, b.Session.EndSHA)

	res, err := synth.Run(ctx, env.Cfg, b, diff, p)
	if err != nil {
		return err
	}
	if *print {
		fmt.Print(res.Markdown)
		return nil
	}
	if err := env.Store.WriteFile(id, "synthesis.md", []byte(res.Markdown)); err != nil {
		return err
	}
	if err := claims.Save(env.Store.ClaimsPath(id), res.Claims); err != nil {
		return err
	}
	fmt.Printf("synthesis by %s → %s\n", res.Provider, rel(env, env.Store.SynthesisPath(id)))
	exec := 0
	for _, c := range res.Claims {
		if c.Executable {
			exec++
		}
	}
	fmt.Printf("%d claims (%d executable, %d trust-me assertions) → %s\n",
		len(res.Claims), exec, len(res.Claims)-exec, rel(env, env.Store.ClaimsPath(id)))
	return nil
}

func providerFor(name string, env *Env, b *bundle.Bundle) (synth.Provider, error) {
	switch strings.ToLower(name) {
	case "", "offline", "none", "mechanical":
		return &synth.Offline{Bundle: b}, nil
	case "anthropic":
		return synth.NewAnthropic(env.Cfg.Synthesis.Model)
	}
	return nil, fmt.Errorf("unknown synthesis provider %q", name)
}

func cmdStale(ctx context.Context, env *Env, args []string) error {
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(args))
	if err != nil {
		return err
	}
	findings, claimsErr := stale.Check(env.Cfg, env.Reg, env.Store.ClaimsPath(id))

	// A reading goes stale the same way a claim does, and for the same reason:
	// prose about code that has since moved is worse than no prose, because it
	// still reads as current.
	var readings []interpret.Finding
	if b, err := env.Store.Load(id); err == nil {
		if file, err := interpret.Load(env.Store.Dir(id)); err == nil {
			readings = file.Stale(currentFingerprints(env, b))
		}
	}

	if claimsErr != nil && len(readings) == 0 {
		return fmt.Errorf("nothing to check for session %s — run `plum synth` or `plum interpret` first", id)
	}
	if len(findings) == 0 && len(readings) == 0 {
		fmt.Println("all claims and readings still address the code they were written against")
		return nil
	}
	for _, f := range findings {
		fmt.Printf("STALE CLAIM    %s  %s\n", f.ID, f.Claim)
		fmt.Printf("               %s — %s\n", f.Symbol, f.Reason)
	}
	for _, r := range readings {
		subject := string(r.Scope)
		if r.Subject != "" {
			subject += " " + r.Subject
		}
		fmt.Printf("STALE READING  %s\n", subject)
		for _, id := range r.Moved {
			fmt.Printf("               %s changed since it was written\n", id)
		}
	}
	os.Exit(1)
	return nil
}

// cmdTrace instruments only the changed symbols and runs the suite. The AST pass
// already determined the instrumentation set (spec §9.1).
func cmdTrace(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	cmdOverride := fs.String("cmd", "", "override the test command for this run")
	keep := fs.Bool("keep", false, "keep the instrumented scratch copy for inspection")
	chain := fs.String("chain", "hottest", "representative chain: hottest|slowest|raising")
	frames := fs.Int("frames", trace.DefaultMaxFrames, "how many frames to render (0 = all)")
	if err := fs.Parse(args); err != nil {
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
	testCmd := env.Cfg.Trace.TestCommand
	if *cmdOverride != "" {
		testCmd = *cmdOverride
	}
	scratch := filepath.Join(store.StateDir(env.Cfg.Root), "scratch", id)
	// The collector instruments through whatever the configured adapters declare,
	// so it stays the same file whether this repo is Go, Python or TypeScript.
	var instrumenters []trace.Instrumenter
	for _, a := range env.Reg.All() {
		instrumenters = append(instrumenters, a)
	}
	context, err := contextSymbols(env, b)
	if err != nil {
		return err
	}
	c := &trace.Collector{
		Root: env.Cfg.Root, Scratch: scratch, Adapters: instrumenters, Context: context,
		TestCommand: testCmd, MaxEvents: env.Cfg.Trace.MaxEvents, Out: os.Stdout,
	}
	if len(context) > 0 {
		fmt.Printf("instrumenting %d changed symbols in full, %d surrounding for structure; running: %s\n",
			len(b.Symbols), len(context), testCmd)
	} else {
		fmt.Printf("instrumenting %d changed symbols, running: %s\n", len(b.Symbols), testCmd)
	}
	res, err := c.Run(ctx, b)
	if err != nil {
		return err
	}
	langs := ""
	if len(res.Languages) > 0 {
		langs = " (" + strings.Join(res.Languages, ", ") + ")"
	}
	fmt.Printf("instrumented %d symbols%s, recorded %d events\n", len(res.Instrumented), langs, len(res.Events))
	for _, s := range res.Skipped {
		fmt.Println("  skipped:", s)
	}
	if res.TestErr != nil {
		fmt.Println("  the suite exited non-zero — recorded execution is still evidence")
	}
	if len(res.Events) == 0 {
		fmt.Println()
		fmt.Println("no events: the changed symbols were never called by the test command.")
		fmt.Println("that is itself a finding — nothing in the suite exercises this session's code.")
		if !*keep {
			_ = os.RemoveAll(scratch)
		}
		return nil
	}

	if err := os.MkdirAll(env.Store.TracesDir(id), 0o755); err != nil {
		return err
	}
	if err := trace.WriteFile(env.Store.TracePath(id), res.Events); err != nil {
		return err
	}
	l := trace.DeriveChainN(res.Events, b, trace.Chain(*chain), *frames)
	if err := l.Save(env.Store.LandscapePath(id)); err != nil {
		return err
	}
	if !*keep {
		_ = os.RemoveAll(scratch)
	} else {
		fmt.Println("  scratch copy kept at", scratch)
	}
	fmt.Println("traces →", rel(env, env.Store.TracePath(id)))
	fmt.Println("landscape →", rel(env, env.Store.LandscapePath(id)))
	printTests(trace.Tests(res.Events))
	printLandscape(l)
	return nil
}

// printTests lists what each test actually reached. A test is the unit a
// developer already thinks in, so it is the unit the recording is reported in.
func printTests(runs []trace.TestRun) {
	if len(runs) == 0 {
		return
	}
	fmt.Println()
	for _, r := range runs {
		flag := ""
		if r.Raised {
			flag = "  ⚠ raised"
		}
		fmt.Printf("  %-38s %2d frames  %d symbols  depth %d%s\n",
			r.Name, r.Frames, len(r.Symbols), r.MaxDepth, flag)
	}
}

// cmdTests lists the recording one test at a time, and what each reached.
func cmdTests(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("tests", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "list the symbols each test reached")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(fs.Args()))
	if err != nil {
		return err
	}
	events, err := trace.ReadFile(env.Store.TracePath(id))
	if err != nil {
		return fmt.Errorf("no traces for %s — run `plum trace` first", id)
	}
	runs := trace.Tests(events)
	if len(runs) == 0 {
		fmt.Println("no tests recorded")
		return nil
	}
	for _, r := range runs {
		flag := ""
		if r.Raised {
			flag = "  ⚠ raised"
		}
		fmt.Printf("%-38s %2d frames  %d symbols  depth %d%s\n",
			r.Name, r.Frames, len(r.Symbols), r.MaxDepth, flag)
		if *verbose {
			for _, sym := range r.Symbols {
				fmt.Printf("    %s\n", sym)
			}
		}
	}
	fmt.Println()
	fmt.Printf("explore one of them: plum explore -test %q\n", runs[0].Name)
	return nil
}

// contextSymbols is the surrounding code recorded for structure only: the other
// declarations in the files this session changed, and optionally their
// neighbours in the same directory.
//
// A change is only legible inside the system it perturbs. Recording that system
// as deeply as the change would cost more and say less, so these frames are
// entered and left with nothing captured — enough to draw the path the change
// sits on.
func contextSymbols(env *Env, b *bundle.Bundle) ([]bundle.SymbolID, error) {
	scope := strings.ToLower(env.Cfg.Trace.Context)
	if scope == "off" || scope == "none" || scope == "" {
		return nil, nil
	}
	changed := map[bundle.SymbolID]bool{}
	dirs := map[string]bool{}
	files := map[string]bool{}
	for _, s := range b.Symbols {
		changed[s.ID] = true
		if s.Kind == "config_key" {
			continue
		}
		files[s.File] = true
		dirs[filepath.Dir(s.File)] = true
	}

	candidates := files
	if scope == "dir" {
		candidates = map[string]bool{}
		for dir := range dirs {
			entries, err := os.ReadDir(filepath.Join(env.Cfg.Root, dir))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					candidates[filepath.ToSlash(filepath.Join(dir, e.Name()))] = true
				}
			}
		}
	}

	var out []bundle.SymbolID
	for path := range candidates {
		if env.Cfg.Excluded(path) {
			continue
		}
		a := env.Reg.For(path)
		if a == nil {
			continue
		}
		src, err := os.ReadFile(filepath.Join(env.Cfg.Root, path))
		if err != nil {
			continue
		}
		syms, err := a.ParseSymbols(path, src)
		if err != nil {
			continue
		}
		for _, s := range syms {
			if changed[s.ID] || (s.Kind != "func" && s.Kind != "method") {
				continue
			}
			if s.Name == "init" || s.Name == "main" {
				continue
			}
			out = append(out, s.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func cmdLandscape(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("landscape", flag.ContinueOnError)
	chain := fs.String("chain", "", "re-derive from stored events: hottest|slowest|raising")
	testName := fs.String("test", "", "draw the path of one named test")
	frames := fs.Int("frames", trace.DefaultMaxFrames, "how many frames to render (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(fs.Args()))
	if err != nil {
		return err
	}
	if *testName != "" {
		b, err := env.Store.Load(id)
		if err != nil {
			return err
		}
		events, err := trace.ReadFile(env.Store.TracePath(id))
		if err != nil {
			return fmt.Errorf("no traces for %s — run `plum trace` first", id)
		}
		scoped, err := scopeToTest(events, *testName)
		if err != nil {
			return err
		}
		policy := trace.Chain(*chain)
		if policy == "" {
			policy = trace.ChainHottest
		}
		l := trace.DeriveChainN(scoped, b, policy, *frames)
		l.TestID = *testName
		if err := l.Save(env.Store.LandscapePath(id)); err != nil {
			return err
		}
		fmt.Printf("landscape for test %q\n", *testName)
		printLandscape(l)
		return nil
	}
	if *chain != "" {
		// Re-deriving from the stored events is cheap; re-running the suite is not.
		b, err := env.Store.Load(id)
		if err != nil {
			return err
		}
		events, err := trace.ReadFile(env.Store.TracePath(id))
		if err != nil {
			return fmt.Errorf("no traces for %s — run `plum trace` first", id)
		}
		l := trace.DeriveChainN(events, b, trace.Chain(*chain), *frames)
		if err := l.Save(env.Store.LandscapePath(id)); err != nil {
			return err
		}
		printLandscape(l)
		return nil
	}
	l, err := loadLandscape(env, id)
	if err != nil {
		return fmt.Errorf("no landscape for %s — run `plum trace` first", id)
	}
	printLandscape(*l)
	return nil
}

// printLandscape draws the round trip in the terminal: descent is entering a
// call, ascent is returning, and the path must close.
func printLandscape(l trace.Landscape) {
	fmt.Println()
	if len(l.Wells) == 0 {
		fmt.Println("(empty landscape)")
		return
	}
	barrierAt := map[int]trace.Barrier{}
	for _, b := range l.Barriers {
		barrierAt[b.ToIdx] = b
	}
	for i, w := range l.Wells {
		indent := strings.Repeat("  ", w.Depth)
		marks := ""
		if w.Context {
			marks = " ·context"
		} else if w.Doc == "" {
			marks += " ·undocumented"
		}
		if w.Risk {
			marks += " ·risk"
		}
		if b, ok := barrierAt[i]; ok {
			arrow := map[string]string{"descend": "↓", "ascend": "↑", "unwind": "⇊"}[b.Direction]
			cost := time.Duration(b.CostNanos).Round(time.Microsecond)
			rat := b.Rationale
			if rat == "" && b.Height >= 0.6 && b.Direction == "descend" {
				rat = "(unexplained)"
			}
			if rat != "" {
				rat = "  “" + oneLine(rat) + "”"
			}
			frames := ""
			if b.Frames > 1 {
				frames = fmt.Sprintf(" ×%d frames", b.Frames)
			}
			fmt.Printf("%s%s %s %s%s%s\n", indent, arrow, cost, b.Kind, frames, rat)
		}
		phase := ""
		switch w.Phase {
		case "resume":
			phase = " (resumed)"
		case "escape":
			phase = " (panic escaped here)"
		}
		open, close := "[", "]"
		if w.Context {
			open, close = "(", ")" // thin brackets: passed through, not changed
		}
		fmt.Printf("%s%s%s%s%s%s\n", indent, open, w.Label, close, phase, marks)
	}
	fmt.Println()
	for _, n := range l.Notes() {
		fmt.Println("·", strings.ReplaceAll(strings.ReplaceAll(n, "**", ""), "`", ""))
	}
	for _, u := range l.UnannotatedExpensive() {
		fmt.Println("·", strings.ReplaceAll(u, "`", ""))
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func loadLandscape(env *Env, id string) (*trace.Landscape, error) {
	return trace.LoadLandscape(env.Store.LandscapePath(id))
}

// scopeToTest narrows a recording to one test, naming the alternatives when the
// requested test is not among them.
func scopeToTest(events []trace.Event, name string) ([]trace.Event, error) {
	scoped := trace.ForTest(events, name)
	if len(scoped) > 0 {
		return scoped, nil
	}
	var names []string
	for _, r := range trace.Tests(events) {
		names = append(names, r.Name)
	}
	return nil, fmt.Errorf("no test named %q in this recording; recorded: %s", name, strings.Join(names, ", "))
}

// cmdExplore serves the landscape. No score, no gate, no timer (P8).
func cmdExplore(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "listen address")
	noOpen := fs.Bool("no-open", false, "do not open a browser")
	testName := fs.String("test", "", "serve the path of one named test")
	noWatch := fs.Bool("no-watch", false, "do not reload when the session or the source changes")
	if err := fs.Parse(args); err != nil {
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
	var l trace.Landscape
	if got, err := loadLandscape(env, id); err == nil {
		l = *got
	} else {
		fmt.Println("no landscape yet — run `plum trace` to record one; the UI will show the bundle only")
	}
	events, _ := trace.ReadFile(env.Store.TracePath(id))
	if *testName != "" {
		scoped, err := scopeToTest(events, *testName)
		if err != nil {
			return err
		}
		events = scoped
		l = trace.DeriveChainN(events, b, trace.ChainHottest, trace.DefaultMaxFrames)
		l.TestID = *testName
		fmt.Printf("serving the path of test %q\n", *testName)
	}
	cs, _ := claims.Load(env.Store.ClaimsPath(id))
	synthesis, _ := os.ReadFile(env.Store.SynthesisPath(id))

	opts := server.Config{
		JournalDir: env.Cfg.Repo.JournalDir,
		ClaimsPath: env.Store.ClaimsPath(id),
		SessionDir: env.Store.Dir(id),
		Adapters:   env.Reg,
		TestFilter: *testName,
		Watch:      !*noWatch,
	}
	switch env.Cfg.Ask.Route {
	case "tmux":
		if ask.Available(ctx) {
			opts.Ask = ask.NewStore(env.Cfg.Root)
			opts.Bridge = &ask.Tmux{Target: env.Cfg.Ask.TmuxTarget}
			target := env.Cfg.Ask.TmuxTarget
			if target == "" {
				if pane, err := ask.FindPane(ctx, env.Cfg.Root); err == nil {
					target = pane.Target + " (" + pane.Agent() + ")"
				} else {
					target = "no agent pane found yet — " + err.Error()
				}
			}
			fmt.Println("questions route to tmux:", target)
		} else {
			fmt.Println("tmux is not running; questions will return the assembled context only")
		}
	case "api", "anthropic":
		if ap, err := synth.NewAnthropic(env.Cfg.Synthesis.Model); err == nil {
			opts.Provider = ap
		} else {
			fmt.Println("ask route is \"api\" but:", err)
		}
	}
	tel := explore.NewStore(store.StateDir(env.Cfg.Root))
	s := server.New(env.Cfg, b, l, events, cs, string(synthesis), tel, opts)
	return s.Serve(ctx, *addr, !*noOpen)
}

// cmdQuiz is available only after the explore phase ended (P8): testing before a
// first encounter is not retrieval practice, it is just failure.
func cmdQuiz(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("quiz", flag.ContinueOnError)
	n := fs.Int("n", 6, "maximum questions")
	force := fs.Bool("force", false, "skip the explore-first requirement")
	if err := fs.Parse(args); err != nil {
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
	tel := explore.NewStore(store.StateDir(env.Cfg.Root))
	if !tel.IsDone(id) && !*force {
		return fmt.Errorf("explore this session first: `plum explore %s`, then click \"I have met this code\" (P8; -force overrides)", id)
	}
	l, err := loadLandscape(env, id)
	if err != nil {
		return fmt.Errorf("no landscape for %s — questions are graded against recorded execution, so run `plum trace` first (P4)", id)
	}
	events, err := trace.ReadFile(env.Store.TracePath(id))
	if err != nil {
		return err
	}
	tele, _ := tel.Load(id)
	targets := explore.TargetSymbols(tele, *l, 6)
	qs := quiz.Generate(targets, events, *l, *n)
	if len(qs) == 0 {
		qs = quiz.Generate(nil, events, *l, *n)
	}
	if len(qs) == 0 {
		return fmt.Errorf("no recorded invocation produced a gradeable question")
	}

	fmt.Printf("%d questions, each graded against a recorded execution of session %s.\n", len(qs), id)
	fmt.Println("no partial credit for plausible — the answer is whatever actually happened.")
	fmt.Println()

	in := bufio.NewScanner(os.Stdin)
	correct := 0
	for i, q := range qs {
		fmt.Printf("%d/%d  [%s]  %s\n", i+1, len(qs), q.Kind, q.Prompt)
		for j, o := range q.Options {
			fmt.Printf("     %c) %s\n", 'a'+j, o)
		}
		fmt.Print("  > ")
		if !in.Scan() {
			fmt.Println()
			break
		}
		given := strings.TrimSpace(in.Text())
		if len(q.Options) > 0 && len(given) == 1 && given[0] >= 'a' && int(given[0]-'a') < len(q.Options) {
			given = q.Options[given[0]-'a']
		}
		if quiz.Grade(q, given) {
			correct++
			fmt.Printf("  ✓ %s\n\n", q.Source)
		} else {
			fmt.Printf("  ✗ recorded: %s\n", q.Expected)
			fmt.Printf("    %s\n\n", q.Source)
			_ = tel.AppendMiss(quiz.MissFor(id, q, given))
		}
	}
	fmt.Printf("%d/%d against recorded execution.\n", correct, len(qs))
	if misses, err := tel.LoadMisses(); err == nil && len(misses) > 0 {
		fmt.Println()
		fmt.Println("across every session in this repo:")
		for _, s := range quiz.Summarise(misses) {
			fmt.Println(" ·", s)
		}
	}
	_ = b
	return nil
}

func cmdClaims(ctx context.Context, env *Env, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(args))
	if err != nil {
		return err
	}
	cs, err := claims.Load(env.Store.ClaimsPath(id))
	if err != nil {
		return fmt.Errorf("no claims for %s — run `plum synth` first", id)
	}
	switch sub {
	case "list":
		for _, c := range cs {
			kind := "assertion "
			if c.Executable {
				kind = "executable"
			}
			fmt.Printf("%s  [%s]  %s\n", c.ID, kind, c.Claim)
			fmt.Printf("           %s\n", c.Symbol)
		}
		return nil
	case "verify":
		scratch := filepath.Join(store.StateDir(env.Cfg.Root), "claims", id)
		vs, err := claims.Verify(ctx, env.Cfg.Root, scratch, cs, trace.CopyTree)
		if err != nil {
			return err
		}
		fails := 0
		for _, v := range vs {
			switch v.Status {
			case "pass":
				fmt.Printf("PASS         %s  %s\n", v.Claim.ID, v.Claim.Claim)
			case "fail":
				fails++
				fmt.Printf("FAIL         %s  %s\n", v.Claim.ID, v.Claim.Claim)
				for _, l := range strings.Split(v.Detail, "\n") {
					fmt.Println("             ", l)
				}
			case "unverifiable":
				fmt.Printf("TRUST-ME     %s  %s\n", v.Claim.ID, v.Claim.Claim)
			default:
				fmt.Printf("SKIP         %s  %s (%s)\n", v.Claim.ID, v.Claim.Claim, v.Detail)
			}
		}
		_ = os.RemoveAll(scratch)
		if fails > 0 {
			fmt.Printf("\n%d claims failed. The doc is wrong or the code is — both worth knowing.\n", fails)
			os.Exit(1)
		}
		return nil
	}
	return fmt.Errorf("usage: plum claims list|verify [session]")
}

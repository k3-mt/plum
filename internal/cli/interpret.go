package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/ask"
	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/interpret"
	"github.com/k3-mt/plum/internal/server"
	"github.com/k3-mt/plum/internal/synth"
	"github.com/k3-mt/plum/internal/trace"
)

// cmdInterpret asks what the change is FOR — the one thing the recording cannot
// supply. It runs over the mechanical narration rather than instead of it, and
// stores the answer with the fingerprints of everything it describes, so it goes
// stale the moment the code moves.
func cmdInterpret(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("interpret", flag.ContinueOnError)
	testName := fs.String("test", "", "interpret one test's path")
	symbol := fs.String("symbol", "", "interpret one symbol")
	show := fs.Bool("show", false, "print the stored reading without asking for a new one")
	refresh := fs.Bool("refresh", false, "ask again even if a reading is stored")
	route := fs.String("route", "", "override the route: tmux | api | print")
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

	scope, subject := interpret.ScopeSession, ""
	switch {
	case *symbol != "":
		scope, subject = interpret.ScopeSymbol, *symbol
	case *testName != "":
		scope, subject = interpret.ScopeTest, *testName
	}

	file, err := interpret.Load(env.Store.Dir(id))
	if err != nil {
		return err
	}
	key := interpret.Entry{Scope: scope, Subject: subject}.Key()

	if existing, ok := file.Entries[key]; ok && !*refresh {
		printEntry(existing, staleNote(file, env, b, key))
		if *show {
			return nil
		}
		fmt.Println()
		fmt.Println("this is the stored reading — `plum interpret -refresh` asks again")
		return nil
	}
	if *show {
		return fmt.Errorf("no reading stored for %s in session %s — run `plum interpret` to make one", key, id)
	}

	brief, ids, err := buildBrief(ctx, env, b, id, scope, subject)
	if err != nil {
		return err
	}

	chosen := *route
	if chosen == "" {
		chosen = env.Cfg.Ask.Route
	}
	switch chosen {
	case "print":
		fmt.Println(interpret.SystemPrompt)
		fmt.Println()
		fmt.Println(brief)
		return nil
	case "api", "anthropic":
		provider, err := synth.NewAnthropic(env.Cfg.Synthesis.Model)
		if err != nil {
			return err
		}
		answer, err := provider.Complete(ctx, interpret.SystemPrompt, brief)
		if err != nil {
			return err
		}
		return storeAndPrint(env, id, b, scope, subject, ids, answer, provider.Name())
	default:
		return interpretViaTmux(ctx, env, id, b, scope, subject, ids, brief)
	}
}

// buildBrief assembles what the model sees: the narration, then the per-frame
// evidence behind it.
func buildBrief(ctx context.Context, env *Env, b *bundle.Bundle, id string, scope interpret.Scope, subject string) (string, []bundle.SymbolID, error) {
	events, _ := trace.ReadFile(env.Store.TracePath(id))
	cs, _ := claims.Load(env.Store.ClaimsPath(id))

	var l trace.Landscape
	if got, err := loadLandscape(env, id); err == nil {
		l = *got
	}
	if scope == interpret.ScopeTest {
		scoped, err := scopeToTest(events, subject)
		if err != nil {
			return "", nil, err
		}
		l = trace.DeriveChainN(scoped, b, trace.ChainHottest, trace.DefaultMaxFrames)
		l.TestID = subject
		events = scoped
	}

	summary := trace.Summary(l, b)
	var steps []string
	for _, s := range trace.Narrate(l, b) {
		line := s.Text
		if s.Note != "" {
			line += "  (" + s.Note + ")"
		}
		steps = append(steps, line)
	}

	// The frames the reading is about, and therefore what it goes stale against.
	var ids []bundle.SymbolID
	seen := map[bundle.SymbolID]bool{}
	switch scope {
	case interpret.ScopeSymbol:
		ids = []bundle.SymbolID{bundle.SymbolID(subject)}
	default:
		for _, w := range l.Wells {
			if w.Phase == "enter" && !seen[w.Symbol] && b.Has(w.Symbol) {
				seen[w.Symbol] = true
				ids = append(ids, w.Symbol)
			}
		}
		if len(ids) == 0 {
			for _, s := range b.Symbols {
				ids = append(ids, s.ID)
			}
		}
	}

	var evidence strings.Builder
	for i, sym := range ids {
		if i >= 6 {
			fmt.Fprintf(&evidence, "\n(%d further frames omitted for length)\n", len(ids)-i)
			break
		}
		evidence.WriteString(server.AssembleContext(contextInput(env, b, events, cs, id), sym))
		evidence.WriteString("\n---\n\n")
	}
	if scope == interpret.ScopeSymbol && len(steps) == 0 {
		summary = "No recording covers this symbol; the evidence below is structural only."
	}
	return interpret.Brief(scope, subject, summary, steps, evidence.String(), b), ids, nil
}

// interpretViaTmux hands the brief to the agent session already running, the
// same way a question from the explore UI travels.
func interpretViaTmux(ctx context.Context, env *Env, id string, b *bundle.Bundle, scope interpret.Scope, subject string, ids []bundle.SymbolID, brief string) error {
	if !ask.Available(ctx) {
		return fmt.Errorf("no tmux session to ask (set [ask] route = \"api\", or use -route print to get the prompt)")
	}
	st := ask.NewStore(env.Cfg.Root)
	req := ask.Request{
		ID: ask.NextID(time.Now()), SessionID: id, Symbol: bundle.SymbolID(subject),
		Question:  "What is this change for? Follow the structure in the instructions exactly.",
		CreatedAt: time.Now().UTC(), Route: "tmux",
	}
	if err := st.Write(req, interpret.SystemPrompt+"\n\n---\n\n"+brief); err != nil {
		return err
	}
	target, err := (&ask.Tmux{Target: env.Cfg.Ask.TmuxTarget}).Send(ctx, env.Cfg.Root, req)
	if err != nil {
		fmt.Println("could not deliver:", err)
		fmt.Println("the brief is waiting in", rel(env, st.PromptPath(req.ID)))
		return nil
	}
	fmt.Printf("asked %s to interpret this — waiting for the reading…\n", target)

	timeout := time.Duration(env.Cfg.Ask.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	answer, err := st.Wait(waitCtx, req.ID, time.Second)
	if err != nil {
		fmt.Println("no reading yet — it will appear at", rel(env, st.AnswerPath(req.ID)))
		return nil
	}
	return storeAndPrint(env, id, b, scope, subject, ids, answer.Text, "tmux/"+target)
}

func storeAndPrint(env *Env, id string, b *bundle.Bundle, scope interpret.Scope, subject string, ids []bundle.SymbolID, markdown, provider string) error {
	file, err := interpret.Load(env.Store.Dir(id))
	if err != nil {
		return err
	}
	entry := interpret.Entry{
		Scope: scope, Subject: subject,
		Markdown: strings.TrimSpace(markdown), Provider: provider,
		Fingerprints: interpret.FingerprintsFor(b, ids),
		GeneratedAt:  time.Now().UTC(),
	}
	file.Entries[entry.Key()] = entry
	if err := interpret.Save(env.Store.Dir(id), file); err != nil {
		return err
	}
	fmt.Println()
	printEntry(entry, "")
	fmt.Println()
	fmt.Println("stored in", rel(env, interpret.Path(env.Store.Dir(id))),
		"— it goes stale when any of the", len(entry.Fingerprints), "frames it describes changes")
	return nil
}

func printEntry(e interpret.Entry, stale string) {
	fmt.Printf("── a reading, not a record ──  %s · %s\n",
		e.Provider, e.GeneratedAt.Format("2006-01-02 15:04"))
	if stale != "" {
		fmt.Println("⚠ " + stale)
	}
	fmt.Println()
	fmt.Println(e.Markdown)
}

func staleNote(file *interpret.File, env *Env, b *bundle.Bundle, key string) string {
	current := currentFingerprints(env, b)
	for _, f := range file.Stale(current) {
		if f.Key == key {
			return fmt.Sprintf("stale: %s changed since this was written", strings.Join(idStrings(f.Moved), ", "))
		}
	}
	return ""
}

// currentFingerprints re-parses the working tree, so a reading is checked
// against the code as it is now rather than as it was captured.
func currentFingerprints(env *Env, b *bundle.Bundle) map[bundle.SymbolID]string {
	out := map[bundle.SymbolID]string{}
	seen := map[string]bool{}
	for _, s := range b.Symbols {
		if seen[s.File] {
			continue
		}
		seen[s.File] = true
		a := env.Reg.For(s.File)
		if a == nil {
			continue
		}
		src, err := os.ReadFile(env.Cfg.Root + "/" + s.File)
		if err != nil {
			continue
		}
		syms, err := a.ParseSymbols(s.File, src)
		if err != nil {
			continue
		}
		for _, sym := range syms {
			out[sym.ID] = sym.Fingerprint
		}
	}
	return out
}

func idStrings(ids []bundle.SymbolID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

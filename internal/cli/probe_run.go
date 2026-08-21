package cli

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/probe"
	"github.com/k3-mt/plum/internal/server"
	"github.com/k3-mt/plum/internal/store"
	"github.com/k3-mt/plum/internal/trace"
)

// probeRunner builds the closure the window calls to run one test.
//
// It lives here rather than in the server because assembling an instrumented
// run needs the adapter registry, the surrounding-context set and a scratch
// tree — everything `plum trace` already puts together. The server should not
// learn how to do that a second time; it should know only that running a probe
// produces a picture.
func probeRunner(env *Env, b *bundle.Bundle, sessionID string) server.ProbeRunner {
	return func(ctx context.Context, p *probe.Probe) (*server.ProbeRun, error) {
		started := time.Now()

		var instrumenters []trace.Instrumenter
		for _, a := range env.Reg.All() {
			instrumenters = append(instrumenters, a)
		}
		surrounding, err := contextSymbols(env, b)
		if err != nil {
			return nil, err
		}
		// Instrument the package the test lives in, as well as whatever the
		// session changed.
		//
		// Without this a probe only draws when the test happens to reach the
		// last capture's changed set — measured here, 180 of 246 tests recorded
		// nothing at all and showed an empty journey. A test picker that offers
		// you 246 tests and draws for 66 of them is not a picker, and the reason
		// is invisible from the window.
		//
		// The widened set is used for instrumentation only. Colouring still asks
		// the session bundle what changed, so your change stays distinguishable
		// from the code around it.
		wide := widen(env, b, p.Test)
		c := &trace.Collector{
			Root: env.Cfg.Root,
			// A scratch tree of its own. The session's belongs to `plum trace`,
			// and a probe re-running every time you save must not race it or
			// quietly replace what it recorded.
			Scratch:     filepath.Join(store.StateDir(env.Cfg.Root), "probe", p.ID),
			Adapters:    instrumenters,
			Context:     surrounding,
			TestCommand: p.Command,
			MaxEvents:   env.Cfg.Trace.MaxEvents,
			Out:         os.NewFile(0, os.DevNull), // the page is the output here
		}
		res, err := c.Run(ctx, wide)
		out := &server.ProbeRun{DurationMS: time.Since(started).Milliseconds()}
		if err != nil {
			return out, err
		}
		out.DurationMS = time.Since(started).Milliseconds()
		out.Passed = res.TestErr == nil
		out.Output = res.TestOutput
		out.Recorded = len(res.Events)

		// A test that failed still ran, and what it did before it failed is
		// usually the whole question. Draw whatever was recorded.
		scoped := trace.ForTest(res.Events, p.Test)
		if len(scoped) == 0 {
			scoped = res.Events
		}
		// The widened bundle here too, not the session's. A well is marked
		// "context" when the bundle does not hold it, and the page renders that
		// as "surrounding code, recorded for structure only" — which stopped
		// being true the moment these symbols were instrumented in full. Drawing
		// a frame that carries recorded arguments as structure-only is a claim
		// about the evidence, and it would be the wrong one.
		l := trace.DeriveChainN(scoped, wide, trace.ChainHottest, trace.DefaultMaxFrames)
		l.TestID, l.SessionID = p.Test, sessionID
		out.Landscape = l
		out.Narration = trace.Narrate(l, wide)
		out.Summary = trace.Summary(l, wide)
		out.Values = frameValues(l, scoped)
		return out, nil
	}
}

// frameValues pairs each drawn frame with the values its invocation recorded,
// so the page can lay them out as rows rather than read them back out of a
// sentence. Reading them back out of the sentence was the alternative, and a
// renderer that has to parse its own prose is a renderer that will get it wrong
// the first time the prose changes.
func frameValues(l trace.Landscape, events []trace.Event) map[int]server.FrameValues {
	type recorded struct {
		args    map[string]string
		argsOut map[string]string
		result  string
		raised  string
	}
	byID := map[string]*recorded{}
	// Argument order is the order the shim recorded them, which is the order
	// they appear in the signature. A map loses that, so it is kept here.
	order := map[string][]string{}
	for _, e := range events {
		r := byID[e.InvocationID]
		if r == nil {
			r = &recorded{}
			byID[e.InvocationID] = r
		}
		switch e.Kind {
		case "call":
			r.args = e.Args
			for name := range e.Args {
				order[e.InvocationID] = append(order[e.InvocationID], name)
			}
			sort.Strings(order[e.InvocationID])
		case "return":
			r.result = e.Result
			r.argsOut = e.ArgsOut
		case "raise":
			r.raised = e.Exception
		}
	}

	out := map[int]server.FrameValues{}
	for i, w := range l.Wells {
		r := byID[w.InvocationID]
		if r == nil {
			continue
		}
		var fv server.FrameValues
		for _, name := range order[w.InvocationID] {
			fv.Args = append(fv.Args, server.NamedValue{
				Name: name, Value: r.args[name], After: r.argsOut[name],
			})
		}
		fv.Result, fv.Raised = r.result, r.raised
		if len(fv.Args) > 0 || fv.Result != "" || fv.Raised != "" {
			out[i] = fv
		}
	}
	return out
}

// widen returns a bundle carrying the session's changed symbols plus everything
// declared in the package that holds the test being probed.
//
// It is deliberately not merged into the session bundle: that bundle is the
// record of what changed, and everything downstream — the debt meter, the
// report, the landscape's context shading — reads it for exactly that. Widening
// it in place would quietly reclassify a whole package as "changed".
func widen(env *Env, b *bundle.Bundle, test string) *bundle.Bundle {
	dir := packageOf(env, test)
	if dir == "" {
		return b
	}
	entries, err := os.ReadDir(filepath.Join(env.Cfg.Root, dir))
	if err != nil {
		return b
	}
	out := &bundle.Bundle{}
	*out = *b
	seen := map[bundle.SymbolID]bool{}
	for _, sym := range b.Symbols {
		seen[sym.ID] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		rel := filepath.Join(dir, e.Name())
		a := env.Reg.For(rel)
		if a == nil {
			continue
		}
		src, err := os.ReadFile(filepath.Join(env.Cfg.Root, rel))
		if err != nil {
			continue
		}
		syms, err := a.ParseSymbols(rel, src)
		if err != nil {
			continue
		}
		for _, sym := range syms {
			if seen[sym.ID] {
				continue
			}
			seen[sym.ID] = true
			out.Symbols = append(out.Symbols, sym)
		}
	}
	return out
}

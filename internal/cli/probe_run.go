package cli

import (
	"context"
	"os"
	"path/filepath"
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
		res, err := c.Run(ctx, b)
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
		l := trace.DeriveChainN(scoped, b, trace.ChainHottest, trace.DefaultMaxFrames)
		l.TestID, l.SessionID = p.Test, sessionID
		out.Landscape = l
		out.Narration = trace.Narrate(l, b)
		out.Summary = trace.Summary(l, b)
		return out, nil
	}
}

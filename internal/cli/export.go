package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/explore"
	"github.com/k3-mt/plum/internal/lang/dbt"
	"github.com/k3-mt/plum/internal/server"
	"github.com/k3-mt/plum/internal/store"
	"github.com/k3-mt/plum/internal/trace"
)

// cmdExport writes the session as one HTML file that needs nothing running.
//
// `plum explore` is a server because it answers questions and watches the tree.
// Reading is not that. Reading happens in a pull request, in a chat thread, on
// somebody else's laptop six months from now — and none of those can run a
// binary from your machine. So the same page is written out with the evidence
// folded in, and it opens from a file:// URL with no network at all.
func cmdExport(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("o", "", "file to write (default plum-<session>.html beside the session)")
	testName := fs.String("test", "", "export the path of one named test")
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

	var l trace.Landscape
	if got, err := loadLandscape(env, id); err == nil {
		l = *got
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
	}
	cs, _ := claims.Load(env.Store.ClaimsPath(id))
	synthesis, _ := os.ReadFile(env.Store.SynthesisPath(id))

	opts := server.Config{
		JournalDir: env.Cfg.Repo.JournalDir,
		ClaimsPath: env.Store.ClaimsPath(id),
		SessionDir: env.Store.Dir(id),
		Adapters:   env.Reg,
		TestFilter: *testName,
		Watch:      false, // nothing is being served; there is nothing to reload
	}
	if flow, err := dbt.LoadFlow(env.Store.FlowPath(id)); err == nil {
		opts.Flow = flow
	}
	srv := server.New(env.Cfg, b, l, events, cs, string(synthesis),
		explore.NewStore(store.StateDir(env.Cfg.Root)), opts)

	html, err := srv.Export()
	if err != nil {
		return err
	}
	path := *out
	if path == "" {
		path = filepath.Join(env.Store.Dir(id), "plum-"+id+".html")
	}
	if err := os.WriteFile(path, html, 0o644); err != nil {
		return err
	}
	fmt.Printf("exported → %s  (%s, opens with nothing running)\n", rel(env, path), humanBytes(int64(len(html))))
	return nil
}

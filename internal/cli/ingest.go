package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kelalaike/plum/internal/lang/dbt"
	"github.com/kelalaike/plum/internal/trace"
)

// cmdIngest reads a run that already happened.
//
// `plum trace` runs the suite under instrumentation. That is the wrong shape for
// a warehouse: dbt records its own execution in detail, so there is nothing to
// instrument, and every run scans billed bytes, so a tool that re-runs your
// project in order to look at it shows up on the invoice. This reads the
// artifacts instead and never triggers anything.
func cmdIngest(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	target := fs.String("target", "target", "the dbt target directory holding manifest.json and run_results.json")
	chain := fs.String("chain", "hottest", "which lineage to draw: hottest|slowest|raising")
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

	dir := *target
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(env.Cfg.Root, dir)
	}
	manifest, results, err := dbt.LoadRun(dir)
	if err != nil {
		return err
	}

	events := dbt.Events(manifest, results)
	if len(events) == 0 {
		fmt.Println("the run recorded no buildable nodes — nothing to draw")
		return nil
	}

	if err := os.MkdirAll(env.Store.TracesDir(id), 0o755); err != nil {
		return err
	}
	if err := trace.WriteFile(env.Store.TracePath(id), events); err != nil {
		return err
	}
	l := trace.DeriveChainN(events, b, trace.Chain(*chain), trace.DefaultMaxFrames)
	if err := l.Save(env.Store.LandscapePath(id)); err != nil {
		return err
	}

	// What the run actually cost is the thing a warehouse reader wants first.
	var bytes, rows int64
	failed, skipped := 0, 0
	for _, res := range results.Results {
		bytes += res.Adapter.BytesProcessed
		rows += res.Adapter.RowsAffected
		switch res.Status {
		case "error", "fail", "runtime error":
			failed++
		case "skipped":
			skipped++
		}
	}
	fmt.Printf("ingested %d nodes from %s\n", len(results.Results), rel(env, dir))
	fmt.Printf("  %.1fs elapsed", results.ElapsedTime)
	if bytes > 0 {
		fmt.Printf(", %s scanned", humanBytes(bytes))
	}
	if rows > 0 {
		fmt.Printf(", %s rows written", commas(rows))
	}
	fmt.Println()
	if failed > 0 || skipped > 0 {
		fmt.Printf("  %d failed, %d skipped because something upstream did\n", failed, skipped)
	}
	fmt.Println("traces →", rel(env, env.Store.TracePath(id)))
	fmt.Println("landscape →", rel(env, env.Store.LandscapePath(id)))
	printTests(trace.Tests(events))
	printLandscape(l)
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

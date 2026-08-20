package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/lang/dbt"
)

// cmdFlow prints the dataflow picture for a warehouse session.
func cmdFlow(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("flow", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(fs.Args()))
	if err != nil {
		return err
	}
	f, err := dbt.LoadFlow(env.Store.FlowPath(id))
	if err != nil {
		return fmt.Errorf("no flow for %s — run `plum ingest` first", id)
	}
	printFlow(f)
	return nil
}

// printFlow reads bottom-up, in build order: every table appears after
// everything it reads. The arrows carry the two facts a warehouse reader wants
// and a call graph cannot hold — how the rows were matched, and how many of
// them came through.
func printFlow(f *dbt.Flow) {
	fmt.Println()
	if len(f.Nodes) == 0 {
		fmt.Println("(nothing in the DAG)")
		return
	}
	head := "build order · " + time.Duration(f.ElapsedNanos).Round(time.Second/10).String()
	if f.BytesScanned > 0 {
		head += " · " + humanBytes(f.BytesScanned) + " scanned"
	}
	if f.RowsWritten > 0 {
		head += " · " + commas(f.RowsWritten) + " rows written"
	}
	if f.Failing > 0 {
		head += fmt.Sprintf(" · %d %s failing", f.Failing, "test"+plural(f.Failing))
	}
	fmt.Println(head)

	inbound := map[bundle.SymbolID][]dbt.FlowLink{}
	for _, l := range f.Links {
		inbound[l.To] = append(inbound[l.To], l)
	}

	layer := -1
	for _, n := range f.Nodes {
		if n.Layer != layer {
			layer = n.Layer
			fmt.Printf("\nlayer %d\n", layer)
		}
		name := n.Name
		if n.Changed {
			name += " *"
		}
		fmt.Printf("  %-34s %s\n", name, nodeFacts(n))

		switch {
		case n.Unresolved != "" && n.Grain != "":
			fmt.Printf("      one row per %s, says the doc — but the SQL %s\n", n.Grain, n.Unresolved)
		case n.Unresolved != "":
			fmt.Printf("      grain unreadable — the SQL %s\n", n.Unresolved)
		case n.Grain != "":
			fmt.Printf("      one row per %s (%s)\n", n.Grain, n.GrainFrom)
		}
		for _, l := range inbound[n.Symbol] {
			rows := ""
			if l.Rows > 0 {
				rows = commas(l.Rows) + " rows"
			}
			fmt.Printf("      ← %-30s %14s  %s\n", l.FromName, rows, linkNote(l))
		}
		if n.Filter != "" {
			fmt.Printf("      %-30s %14s  %s\n", "where "+trunc(n.Filter, 28), "", "drops rows that do not match")
		}
		if len(n.Aggregates) > 0 {
			fmt.Printf("      %-30s %14s  %s\n", strings.Join(n.Aggregates, ", "), "", "aggregates: many rows become one")
		}
		for _, t := range n.Tests {
			if t.Status == "fail" || t.Status == "error" {
				fmt.Printf("      ✗ %-28s %14s  %s\n", t.Name, commas(int64(t.Failures))+" rows", "failed")
				continue
			}
			fmt.Printf("      ✓ %s\n", t.Name)
		}
		for _, r := range n.Risks {
			fmt.Printf("      ! %s\n", oneLine(r))
		}
	}

	if len(f.Findings) > 0 {
		fmt.Println()
		for _, note := range f.Findings {
			fmt.Println("·", note)
		}
	}
	fmt.Println()
}

func nodeFacts(n dbt.FlowNode) string {
	var parts []string
	switch n.Kind {
	case "source":
		parts = append(parts, "source")
	case "outside":
		parts = append(parts, "outside the DAG")
	default:
		m := n.Materialized
		if m == "" {
			m = "model"
		}
		if n.UniqueKey != "" {
			m += " on " + n.UniqueKey
		}
		parts = append(parts, m)
	}
	if n.Status == "not-run" && n.Kind == "model" {
		parts = append(parts, "not rebuilt in this run")
	}
	if n.Rows > 0 {
		parts = append(parts, commas(n.Rows)+" rows")
	}
	if n.Bytes > 0 {
		parts = append(parts, humanBytes(n.Bytes))
	}
	if n.Nanos > 0 {
		parts = append(parts, time.Duration(n.Nanos).Round(time.Second/10).String())
	}
	return strings.Join(parts, " · ")
}

func linkNote(l dbt.FlowLink) string {
	if !l.InDAG {
		return relationLabel(l) + " — written into the SQL, invisible to dbt"
	}
	note := relationLabel(l)
	if l.Note != "" {
		note += " — " + l.Note
	}
	return note
}

func relationLabel(l dbt.FlowLink) string {
	switch l.Relation {
	case "from":
		return "from"
	case "ref":
		return "declared in the DAG, not found in the statement"
	}
	if l.Key != "" {
		return l.Relation + " join on " + l.Key
	}
	if l.On != "" {
		return l.Relation + " join on " + trunc(l.On, 40)
	}
	return l.Relation + " join"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

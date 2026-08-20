package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/server"
	"github.com/kelalaike/plum/internal/synth"
	"github.com/kelalaike/plum/internal/trace"
)

// cmdContext prints the mechanically-assembled evidence to stdout, so it can be
// piped into any tool that takes text.
//
// This is the same context `plum ask` sends and the explore UI shows: exact
// declaration source, real recorded arguments and return values, callers and
// callees with their code, risk markers, journalled rationale, claims. It is
// assembled from the bundle rather than retrieved by a search, so it does not
// depend on a model guessing which files to open — and it is deterministic
// given a commit range, so the same question produces the same context.
func cmdContext(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	symbol := fs.String("symbol", "", "one symbol; omit for the whole session")
	session := fs.String("session", "", "session id (default: latest)")
	asJSON := fs.Bool("json", false, "emit JSON instead of markdown")
	withDiff := fs.Bool("diff", false, "include the diff (session brief only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := env.Store.Resolve(*session)
	if err != nil {
		return err
	}
	b, err := env.Store.Load(id)
	if err != nil {
		return err
	}
	events, _ := trace.ReadFile(env.Store.TracePath(id))
	cs, _ := claims.Load(env.Store.ClaimsPath(id))

	if *symbol == "" {
		if *asJSON {
			return writeIndentedJSON(b)
		}
		diff := ""
		if *withDiff {
			diff, _ = env.Repo.Diff(ctx, b.Session.StartSHA, b.Session.EndSHA)
		}
		fmt.Print(synth.Brief(b, diff, env.Cfg.Synthesis.MaxDiff))
		return nil
	}

	sym := bundle.SymbolID(*symbol)
	if !b.Has(sym) {
		matches := matchSymbols(b, *symbol)
		switch len(matches) {
		case 1:
			sym = matches[0]
		case 0:
			return fmt.Errorf("%s is not in session %s (see `plum report`)", *symbol, id)
		default:
			return fmt.Errorf("%q matches %d symbols: %s", *symbol, len(matches), joinIDs(matches))
		}
	}
	if *asJSON {
		return writeIndentedJSON(server.AssembleContextJSON(env.Cfg, b, events, cs, sym))
	}
	fmt.Print(server.AssembleContext(env.Cfg, b, events, cs, sym))
	return nil
}

// matchSymbols lets a caller name a symbol by its unqualified name when that is
// unambiguous, so `plum context -symbol Cache.get` works.
func matchSymbols(b *bundle.Bundle, want string) []bundle.SymbolID {
	var out []bundle.SymbolID
	for _, s := range b.Symbols {
		if s.Name == want || strings.HasSuffix(string(s.ID), "::"+want) {
			out = append(out, s.ID)
		}
	}
	return out
}

func joinIDs(ids []bundle.SymbolID) string {
	var parts []string
	for _, id := range ids {
		parts = append(parts, string(id))
	}
	return strings.Join(parts, ", ")
}

func writeIndentedJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

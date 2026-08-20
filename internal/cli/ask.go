package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/ask"
	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/server"
	"github.com/k3-mt/plum/internal/trace"
)

// cmdAsk is the terminal door to the same bridge the explore UI uses, so a
// question can be asked from a tmux popup without opening a browser.
func cmdAsk(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	symbol := fs.String("symbol", "", "the symbol the question is about")
	session := fs.String("session", "", "session id (default: latest)")
	wait := fs.Bool("wait", true, "wait for the answer to land")
	keepAs := fs.String("keep", "", "keep the answer: journal, claim, comment (comma-separated)")
	pending := fs.Bool("pending", false, "list questions still waiting for an answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st := ask.NewStore(env.Cfg.Root)

	if *pending {
		reqs := st.Pending()
		if len(reqs) == 0 {
			fmt.Println("nothing waiting")
			return nil
		}
		for _, r := range reqs {
			fmt.Printf("%s  %s\n    %s\n", r.ID, r.Symbol, r.Question)
		}
		return nil
	}

	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return fmt.Errorf(`usage: plum ask -symbol <id> "<question>"`)
	}
	id, err := env.Store.Resolve(*session)
	if err != nil {
		return err
	}
	b, err := env.Store.Load(id)
	if err != nil {
		return err
	}
	if *symbol == "" {
		return fmt.Errorf("which symbol is this about? pass -symbol (see `plum report`)")
	}
	sym := b.Lookup(bundle.SymbolID(*symbol))
	if !b.Has(sym.ID) {
		return fmt.Errorf("%s is not in session %s", *symbol, id)
	}

	// The same mechanically-assembled context the UI sends: source, recorded
	// invocations, edges, risks, rationale, claims. Never a search result.
	events, _ := trace.ReadFile(env.Store.TracePath(id))
	cs, _ := claims.Load(env.Store.ClaimsPath(id))
	contextMD := server.AssembleContext(contextInput(env, b, events, cs, id), sym.ID)

	req := ask.Request{
		ID: ask.NextID(time.Now()), SessionID: id, Symbol: sym.ID,
		Question: question, CreatedAt: time.Now().UTC(), Route: "tmux",
	}
	if err := st.Write(req, contextMD); err != nil {
		return err
	}
	bridge := &ask.Tmux{Target: env.Cfg.Ask.TmuxTarget}
	target, err := bridge.Send(ctx, env.Cfg.Root, req)
	if err != nil {
		fmt.Println("could not deliver the question:", err)
		fmt.Println("the question and its context are waiting in", rel(env, st.PromptPath(req.ID)))
		return nil
	}
	fmt.Printf("question %s sent to %s\n", req.ID, target)
	if !*wait {
		fmt.Println("answer will appear at", rel(env, st.AnswerPath(req.ID)))
		return nil
	}

	timeout := time.Duration(env.Cfg.Ask.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fmt.Println("waiting for the answer (ctrl-c to stop; it will still land in the file)…")
	answer, err := st.Wait(waitCtx, req.ID, time.Second)
	if err != nil {
		fmt.Println("no answer yet — it will appear at", rel(env, st.AnswerPath(req.ID)))
		return nil
	}
	fmt.Println()
	fmt.Println(answer.Text)

	if *keepAs == "" {
		return nil
	}
	var e ask.Enrichment
	for _, part := range strings.Split(*keepAs, ",") {
		switch strings.TrimSpace(part) {
		case "journal":
			e.Journal = true
		case "claim":
			e.Claim = true
		case "comment":
			e.Comment = true
		default:
			return fmt.Errorf("unknown -keep value %q (journal, claim, comment)", part)
		}
	}
	res, err := ask.Keep(env.Cfg.Root, env.Cfg.Repo.JournalDir, env.Store.ClaimsPath(id), req, answer.Text, sym, e)
	if err != nil {
		return err
	}
	fmt.Println()
	for _, n := range res.Notes {
		fmt.Println("·", n)
	}
	return nil
}

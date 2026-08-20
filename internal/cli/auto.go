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
)

// autoState remembers what the last automatic capture saw, so a hook that fires
// after every turn does not produce a session per turn.
type autoState struct {
	LastEndSHA string    `json:"last_end_sha"`
	LastTree   string    `json:"last_tree_hash"`
	LastID     string    `json:"last_session_id"`
	At         time.Time `json:"at"`
}

func autoStatePath(env *Env) string {
	return filepath.Join(env.Cfg.Root, ".plum", "auto-state.json")
}

func loadAutoState(env *Env) autoState {
	var st autoState
	data, err := os.ReadFile(autoStatePath(env))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

func saveAutoState(env *Env, st autoState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(autoStatePath(env), append(data, '\n'), 0o644)
}

// cmdAuto is what the hooks call. It captures a session if anything actually
// changed, links it to the commit with a git note, and — only when the gate
// fires — runs whatever heavier work the config asks for.
//
// It never fails loudly and never blocks: a hook that breaks a commit or an
// agent session is a hook that gets uninstalled within the hour.
func cmdAuto(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("auto", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "print nothing on success")
	asJSON := fs.Bool("json", false, "emit a Claude Code hook response on stdout")
	force := fs.Bool("force", false, "capture even if nothing changed since the last one")
	if err := parseArgs(fs, args); err != nil {
		return nil // a hook must not fail on a bad flag
	}
	// Hooks are fed JSON on stdin and nothing here needs it. It is deliberately
	// not read: stdin may be an inherited pipe that never closes, and a hook
	// that blocks forever is worse than one that never runs.

	// Run from a hook this says nothing — after every agent turn, most of these
	// are the normal answer and noise is how a hook gets uninstalled. Run by
	// hand it has to explain itself: exiting 0 with no output and no session is
	// indistinguishable from being broken.
	why := func(reason string) error {
		if !*quiet && !*asJSON {
			fmt.Println("plum: nothing captured —", reason)
		}
		return nil
	}

	if !env.Cfg.Auto.Enabled {
		return why("automatic capture is off ([auto] enabled in .plum/config.toml)")
	}
	if !env.Repo.HasCommits(ctx) {
		return why("this repository has no commits yet, so there is nothing to diff against")
	}

	head, err := env.Repo.RevParse(ctx, "HEAD")
	if err != nil {
		return nil
	}
	tree, _ := env.Repo.WorkingTreeID(ctx, config.Dir)
	st := loadAutoState(env)

	// Debounce on content, not on time. The Stop hook fires after every turn,
	// and most turns change nothing worth capturing.
	if !*force && st.LastTree == tree && st.LastTree != "" {
		return why("the tree is byte-for-byte what the last capture already recorded (-force overrides)")
	}
	from := startPoint(ctx, env, st)
	if from == "" {
		return why("no starting point: nothing recorded where the last session ended")
	}

	started := time.Now().UTC()
	sess, err := capture.Close(ctx, env.Cfg, env.Repo, from, started, "auto", "claude-code")
	if err != nil {
		return nil
	}
	if sess.StartSHA == sess.EndSHA {
		saveAutoState(env, autoState{LastEndSHA: sess.EndSHA, LastTree: tree, LastID: st.LastID, At: started})
		return why("the range is empty: nothing has changed since the last capture")
	}
	j, _ := journal.Harvest(filepath.Join(env.Cfg.Root, env.Cfg.Repo.JournalDir), time.Time{})
	sess.JournalPresent = len(j) > 0

	b, err := extract.New(env.Repo, env.Cfg, env.Reg).Extract(ctx, *sess, j)
	if err != nil {
		return nil
	}
	if len(b.Symbols) == 0 && len(b.Files) == 0 {
		saveAutoState(env, autoState{LastEndSHA: sess.EndSHA, LastTree: tree, LastID: st.LastID, At: started})
		return why("the change touched nothing this repository is configured to analyse " +
			"(see [repo] languages and exclude)")
	}
	if err := env.Store.Save(b); err != nil {
		return nil
	}
	// Tile from where this capture ended, not from HEAD: uncommitted work would
	// otherwise be re-captured in full by every turn that touches anything.
	if err := saveAutoState(env, autoState{
		LastEndSHA: b.Session.EndSHA, LastTree: tree, LastID: b.Session.ID, At: started,
	}); err != nil {
		return nil
	}

	// Attach the analysis to the commit, so a commit can be explored later
	// without remembering which session it was.
	if head != "" {
		_ = env.Repo.NoteAdd(ctx, head, noteFor(b))
	}

	if b.Gate.Fired {
		runOnGate(ctx, env, b)
	}

	summary := summarise(b)
	switch {
	case *asJSON:
		emitHookResponse(env, b, summary)
	case !*quiet:
		fmt.Println(summary)
	}
	return nil
}

// startPoint is where this capture's range begins.
//
// Normally it is wherever the last automatic capture ended, so consecutive
// captures tile the history rather than overlapping. With no prior state the
// answer depends on what just happened: a commit landed, so the range is its
// parent; or work is sitting uncommitted, so the range is HEAD.
func startPoint(ctx context.Context, env *Env, st autoState) string {
	if st.LastEndSHA != "" {
		return st.LastEndSHA
	}
	if dirty, _ := env.Repo.WorkingTreeDirty(ctx); dirty {
		head, _ := env.Repo.RevParse(ctx, "HEAD")
		return head
	}
	if parent, err := env.Repo.RevParse(ctx, "HEAD~1"); err == nil && parent != "" {
		return parent
	}
	// A clean tree at the very first commit: there is no range to take.
	return ""
}

// runOnGate does the expensive work, and only for a session that earned it.
func runOnGate(ctx context.Context, env *Env, b *bundle.Bundle) {
	for _, step := range env.Cfg.Auto.OnGate {
		switch strings.TrimSpace(strings.ToLower(step)) {
		case "trace":
			_ = cmdTrace(ctx, env, []string{b.Session.ID})
		case "synth":
			_ = cmdSynth(ctx, env, []string{b.Session.ID})
		}
	}
}

func noteFor(b *bundle.Bundle) string {
	var w strings.Builder
	fmt.Fprintf(&w, "plum session %s\n", b.Session.ID)
	if b.Gate.Fired {
		fmt.Fprintf(&w, "gate FIRED — %s\n", strings.Join(b.Gate.Reasons, " · "))
	} else {
		w.WriteString("gate clear\n")
	}
	fmt.Fprintf(&w, "%d symbols, %d files\n", len(b.Symbols), len(b.Files))
	fmt.Fprintf(&w, "plum report %s\n", b.Session.ID)
	return w.String()
}

func summarise(b *bundle.Bundle) string {
	if !b.Gate.Fired {
		return fmt.Sprintf("plum: session %s captured — gate clear", b.Session.ID)
	}
	return fmt.Sprintf("plum: session %s — GATE FIRED: %s\n  plum report %s",
		b.Session.ID, strings.Join(b.Gate.Reasons, " · "), b.Session.ID)
}

// emitHookResponse speaks the Claude Code hook protocol: a systemMessage is
// shown to the developer in the agent's own UI, which is where they already are.
func emitHookResponse(env *Env, b *bundle.Bundle, summary string) {
	resp := map[string]any{"suppressOutput": true}
	if b.Gate.Fired && env.Cfg.Auto.Notify {
		resp["systemMessage"] = summary
		resp["suppressOutput"] = false
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	fmt.Println(string(data))
}

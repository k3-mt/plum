package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// An agent that knows plum exists.
//
// `plum hooks` wires capture into one repository. That is the right scope for a
// hook — it runs a command, and the command needs a repository to run in — but
// it is the wrong scope for the agent's own behaviour. Whether Claude offers to
// show you what it just changed is not a fact about the repository; it is a fact
// about how you want your agent to work, everywhere, and it belongs in the file
// the agent reads in every session: ~/.claude/CLAUDE.md, or ~/.codex/AGENTS.md.
//
// So this writes a short, marked block into those files. Marked, so install is
// idempotent and uninstall removes exactly what was put there; short, because
// the block is read by the agent on every turn of every session, and every line
// of it is paid for in context.

const (
	agentStart = "<!-- plum:agent-start -->"
	agentEnd   = "<!-- plum:agent-end -->"
)

// agentBlock is what the agent is told. It is addressed to the agent, in the
// imperative, and it says when to offer and when to stay quiet — the second
// matters more: an agent that runs plum's slow commands unprompted, or sets it
// up in repositories nobody asked about, is an agent whose instructions get
// deleted.
const agentBlock = agentStart + `
## plum — keep the developer's picture of the code current

plum is installed on this machine. It records what you change and what actually
ran, and shows the developer the parts of the code they have not read yet.

- At the start of a session in a git repository, look for ` + "`.plum/config.toml`" + `.
  If it is absent, offer once: "Want me to set up plum here, so you can see what
  I change?" If yes, run ` + "`plum init && plum hooks install`" + `. Never set it up
  without asking.
- When you finish a piece of work that changed code, offer: "Want to see what
  changed, visualised?" If yes, run ` + "`plum watch`" + `. It opens a window that
  stays; running it again raises the same window rather than a second one.
- When the developer asks why something behaves as it does, run
  ` + "`plum context <symbol>`" + ` and reason from the recorded evidence before
  reading source alone.
- Do not run ` + "`plum trace`" + ` or ` + "`plum synth`" + ` unprompted — they are slow.
  Capture is automatic once the hooks are installed.
- ` + "`plum help`" + ` lists every command.
` + agentEnd + "\n"

type agentTarget struct {
	name string // what it is called in output
	path string // the file the agent reads in every session
}

// agentTargets are the files read at the start of every session by each agent.
// Both are created if absent — an empty instructions file is a normal state
// for someone who has not written any — and merged into if present.
func agentTargets() ([]agentTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return []agentTarget{
		{"Claude Code", filepath.Join(home, ".claude", "CLAUDE.md")},
		{"Codex", filepath.Join(home, ".codex", "AGENTS.md")},
	}, nil
}

func cmdAgent(_ context.Context, _ *Env, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	claudeOnly := fs.Bool("claude", false, "only Claude Code")
	codexOnly := fs.Bool("codex", false, "only Codex")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	targets, err := agentTargets()
	if err != nil {
		return err
	}
	var chosen []agentTarget
	for _, t := range targets {
		switch {
		case *claudeOnly && t.name != "Claude Code":
		case *codexOnly && t.name != "Codex":
		default:
			chosen = append(chosen, t)
		}
	}

	switch sub {
	case "install":
		for _, t := range chosen {
			changed, err := installAgentBlock(t.path)
			if err != nil {
				return fmt.Errorf("%s: %w", t.name, err)
			}
			if changed {
				fmt.Printf("%s will offer plum in every session  (%s)\n", t.name, t.path)
			} else {
				fmt.Printf("%s already knows plum  (%s)\n", t.name, t.path)
			}
		}
		fmt.Println()
		fmt.Println("from the next session on, your agent offers to set plum up in a repository")
		fmt.Println("that lacks it, and offers to show you what changed when it finishes work.")
		fmt.Println("it never runs anything slow unprompted.")
		return nil
	case "uninstall":
		for _, t := range chosen {
			removed, err := uninstallAgentBlock(t.path)
			if err != nil {
				return fmt.Errorf("%s: %w", t.name, err)
			}
			if removed {
				fmt.Printf("%s no longer knows plum  (%s)\n", t.name, t.path)
			}
		}
		return nil
	case "status":
		for _, t := range chosen {
			b, err := os.ReadFile(t.path)
			has := err == nil && strings.Contains(string(b), agentStart)
			state := "not told about plum"
			if has {
				state = "offers plum in every session"
			}
			fmt.Printf("%-12s %s  (%s)\n", t.name, state, t.path)
		}
		return nil
	}
	return fmt.Errorf("usage: plum agent install|uninstall|status [-claude] [-codex]")
}

// installAgentBlock puts the block at the end of the file, or replaces the one
// already there. Everything the user wrote is kept byte for byte: this file is
// theirs, and plum is a guest in it.
func installAgentBlock(path string) (changed bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	before := string(b)
	body := before
	if s, e, ok := agentSpan(body); ok {
		body = body[:s] + agentBlock + body[e:]
	} else {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if body != "" {
			body += "\n"
		}
		body += agentBlock
	}
	if body == before {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(body), 0o644)
}

func uninstallAgentBlock(path string) (removed bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	body := string(b)
	s, e, ok := agentSpan(body)
	if !ok {
		return false, nil
	}
	// Take the blank line that install put before the block, so that
	// install+uninstall leaves the file exactly as it was found.
	head := strings.TrimRight(body[:s], "\n")
	if head != "" {
		head += "\n"
	}
	return true, os.WriteFile(path, []byte(head+body[e:]), 0o644)
}

// agentSpan finds the block plum wrote, start marker through the newline after
// the end marker. Only a block with both markers counts: half a marker is
// somebody else's text.
func agentSpan(body string) (start, end int, ok bool) {
	start = strings.Index(body, agentStart)
	if start < 0 {
		return 0, 0, false
	}
	rel := strings.Index(body[start:], agentEnd)
	if rel < 0 {
		return 0, 0, false
	}
	end = start + rel + len(agentEnd)
	if end < len(body) && body[end] == '\n' {
		end++
	}
	return start, end, true
}

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// marker identifies the hooks plum installed, so install is idempotent and
// uninstall removes only what plum put there.
//
// It is written into the command as a trailing shell comment rather than
// inferred from the binary's name: the binary may be installed under any name,
// and detection that depends on it silently breaks both idempotence and
// uninstall.
const marker = "plum-auto-capture"

// cmdHooks wires automatic capture into the two places a session actually ends:
// the agent stopping, and a commit landing.
//
// The gate still decides whether anything heavy runs. Capture is milliseconds —
// it is a commit range and an AST pass — so it always runs; tracing and
// synthesis run only for a session that earned the attention (P6).
func cmdHooks(ctx context.Context, env *Env, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("hooks", flag.ContinueOnError)
	claudeOnly := fs.Bool("claude", false, "only the Claude Code Stop hook")
	gitOnly := fs.Bool("git", false, "only the git post-commit hook")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	both := !*claudeOnly && !*gitOnly

	switch sub {
	case "install":
		if both || *claudeOnly {
			if err := installClaudeHook(env); err != nil {
				return err
			}
		}
		if both || *gitOnly {
			if err := installGitHook(ctx, env); err != nil {
				return err
			}
		}
		fmt.Println()
		fmt.Println("capture now happens on its own. the gate still decides whether")
		fmt.Println("anything heavier runs — see [auto] on_gate in .plum/config.toml.")
		fmt.Println()
		fmt.Println("note: Claude Code loads hooks at startup, so an already-running")
		fmt.Println("session picks this up after /hooks or a restart.")
		return nil
	case "uninstall":
		if both || *claudeOnly {
			if err := uninstallClaudeHook(env); err != nil {
				return err
			}
		}
		if both || *gitOnly {
			if err := uninstallGitHook(ctx, env); err != nil {
				return err
			}
		}
		return nil
	case "status":
		return hooksStatus(ctx, env)
	}
	return fmt.Errorf("usage: plum hooks install|uninstall|status [-claude] [-git]")
}

func claudeSettingsPath(env *Env) string {
	return filepath.Join(env.Cfg.Root, ".claude", "settings.json")
}

// installClaudeHook adds a Stop hook, merging into whatever is already there.
// Settings files are shared with the rest of a team's tooling; replacing one
// wholesale is how you delete someone else's configuration.
func installClaudeHook(env *Env) error {
	path := claudeSettingsPath(env)
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON; fix it before installing hooks: %w", rel(env, path), err)
		}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	stop, _ := hooks["Stop"].([]any)

	for _, entry := range stop {
		if strings.Contains(fmt.Sprint(entry), marker) {
			fmt.Println("kept", rel(env, path), "(plum Stop hook already present)")
			return nil
		}
	}

	binary, err := os.Executable()
	if err != nil {
		binary = "plum"
	}
	stop = append(stop, map[string]any{
		"hooks": []any{map[string]any{
			"type": "command",
			// async so a capture never delays the end of a turn.
			"command":       binary + " auto -json  # " + marker,
			"async":         true,
			"timeout":       60,
			"statusMessage": "plum: capturing the session",
		}},
	})
	hooks["Stop"] = stop
	settings["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", rel(env, path), "— Stop hook")
	return nil
}

func uninstallClaudeHook(env *Env) error {
	path := claudeSettingsPath(env)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("no", rel(env, path))
		return nil
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	var kept []any
	for _, entry := range stop {
		if !strings.Contains(fmt.Sprint(entry), marker) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("removed the plum Stop hook from", rel(env, path))
	return nil
}

func gitHookPath(ctx context.Context, env *Env) (string, error) {
	dir, err := env.Repo.GitDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hooks", "post-commit"), nil
}

// installGitHook is the fallback for work that never went through an agent: a
// commit is a session boundary whoever made it.
func installGitHook(ctx context.Context, env *Env) error {
	path, err := gitHookPath(ctx, env)
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		binary = "plum"
	}
	line := fmt.Sprintf("%s auto -quiet >/dev/null 2>&1 || true   # %s", binary, marker)

	existing, _ := os.ReadFile(path)
	if strings.Contains(string(existing), marker) {
		fmt.Println("kept", rel(env, path), "(plum line already present)")
		return nil
	}
	body := string(existing)
	if body == "" {
		body = "#!/bin/sh\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		return err
	}
	fmt.Println("wrote", rel(env, path), "— post-commit")
	return nil
}

func uninstallGitHook(ctx context.Context, env *Env) error {
	path, err := gitHookPath(ctx, env)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("no post-commit hook")
		return nil
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, marker) {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o755); err != nil {
		return err
	}
	fmt.Println("removed the plum line from", rel(env, path))
	return nil
}

func hooksStatus(ctx context.Context, env *Env) error {
	claude := "not installed"
	if data, err := os.ReadFile(claudeSettingsPath(env)); err == nil && strings.Contains(string(data), marker) {
		claude = "installed (Stop)"
	}
	git := "not installed"
	if path, err := gitHookPath(ctx, env); err == nil {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), marker) {
			git = "installed (post-commit)"
		}
	}
	fmt.Printf("%-24s %s\n", "Claude Code Stop hook", claude)
	fmt.Printf("%-24s %s\n", "git post-commit hook", git)
	fmt.Printf("%-24s %t\n", "auto capture enabled", env.Cfg.Auto.Enabled)
	onGate := "nothing (capture only)"
	if len(env.Cfg.Auto.OnGate) > 0 {
		onGate = strings.Join(env.Cfg.Auto.OnGate, ", ")
	}
	fmt.Printf("%-24s %s\n", "runs when gate fires", onGate)

	st := loadAutoState(env)
	if st.LastID != "" {
		fmt.Printf("%-24s %s at %s\n", "last automatic session", st.LastID, st.At.Format("2006-01-02 15:04:05"))
	}
	return nil
}

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/store"
)

func hookEnv(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default(root)
	return &Env{Cfg: cfg, Store: store.New(cfg)}
}

// A settings file is shared with the rest of a team's tooling. Installing a
// hook must merge into it; replacing it wholesale deletes someone else's config.
func TestInstallingTheStopHookPreservesExistingSettings(t *testing.T) {
	env := hookEnv(t)
	path := claudeSettingsPath(env)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "model": "opus",
  "permissions": {"allow": ["Bash(git *)"]},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}],
    "PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "prettier"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installClaudeHook(env); err != nil {
		t.Fatal(err)
	}

	var settings map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("installing produced invalid JSON: %v", err)
	}
	if settings["model"] != "opus" {
		t.Error("an unrelated setting was lost")
	}
	if _, ok := settings["permissions"]; !ok {
		t.Error("permissions were lost")
	}
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("another event's hooks were lost")
	}
	stop := hooks["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("Stop has %d entries, want the existing one plus plum's", len(stop))
	}
	if !strings.Contains(string(data), "notify-send done") {
		t.Error("the existing Stop hook was replaced")
	}
	if !strings.Contains(string(data), marker) {
		t.Error("plum's hook was not added")
	}

	// Installing twice must not duplicate it.
	if err := installClaudeHook(env); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), marker) != 1 {
		t.Errorf("installing twice duplicated the hook:\n%s", data)
	}

	// Uninstalling removes only plum's entry.
	if err := uninstallClaudeHook(env); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), marker) {
		t.Error("uninstall left plum's hook behind")
	}
	if !strings.Contains(string(data), "notify-send done") {
		t.Error("uninstall removed someone else's hook")
	}
	if !strings.Contains(string(data), "prettier") {
		t.Error("uninstall removed another event's hooks")
	}
}

func TestInstallRefusesToClobberBrokenSettings(t *testing.T) {
	env := hookEnv(t)
	path := claudeSettingsPath(env)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installClaudeHook(env); err == nil {
		t.Fatal("installing over invalid JSON should fail loudly, not overwrite")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{not json" {
		t.Error("the broken file was overwritten instead of reported")
	}
}

// The generated hook must match the shape Claude Code actually reads, or it
// silently never fires — the worst failure mode for automation.
func TestGeneratedHookHasTheRightShape(t *testing.T) {
	env := hookEnv(t)
	if err := installClaudeHook(env); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(claudeSettingsPath(env))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Async   bool   `json:"async"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	stop := settings.Hooks["Stop"]
	if len(stop) != 1 || len(stop[0].Hooks) != 1 {
		t.Fatalf("unexpected shape: %s", data)
	}
	h := stop[0].Hooks[0]
	if h.Type != "command" {
		t.Errorf("type = %q, want command", h.Type)
	}
	if !strings.Contains(h.Command, "auto") {
		t.Errorf("command = %q", h.Command)
	}
	if !h.Async {
		t.Error("the hook must be async, or every turn waits on a capture")
	}
	if h.Timeout <= 0 {
		t.Error("a hook with no timeout can hang a session")
	}
}

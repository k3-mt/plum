package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ~/.claude/CLAUDE.md is the user's own file. plum is a guest in it: it adds one
// marked block, touches nothing else, and leaves on request without a trace.
func TestTheAgentBlockIsAGuestInTheUsersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	theirs := "# Mine\n\nAlways answer in haiku.\n"
	if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := installAgentBlock(path)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), theirs) {
		t.Errorf("the user's text was not kept byte for byte:\n%s", got)
	}
	if !strings.Contains(string(got), agentStart) || !strings.Contains(string(got), agentEnd) {
		t.Error("block is not marked at both ends")
	}
	if !strings.Contains(string(got), "plum watch") {
		t.Error("the block does not tell the agent the one command it is there to offer")
	}

	// Twice is once. A block per install would grow the agent's context on
	// every reinstall, and the second copy would be read on every turn.
	changed, err = installAgentBlock(path)
	if err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v — want a no-op", changed, err)
	}
	if n := strings.Count(string(mustRead(t, path)), agentStart); n != 1 {
		t.Errorf("after two installs the block appears %d times", n)
	}

	removed, err := uninstallAgentBlock(path)
	if err != nil || !removed {
		t.Fatalf("uninstall: removed=%v err=%v", removed, err)
	}
	if after := string(mustRead(t, path)); after != theirs {
		t.Errorf("uninstall did not restore the file:\nwant %q\ngot  %q", theirs, after)
	}
}

// Most people have never written a CLAUDE.md. Install must not make that a
// prerequisite.
func TestInstallCreatesTheFileAndItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "CLAUDE.md")
	changed, err := installAgentBlock(path)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	got := string(mustRead(t, path))
	if got != agentBlock {
		t.Errorf("a fresh file should be exactly the block, got:\n%s", got)
	}
}

// An older block is replaced in place, not appended after. The agent reads the
// whole file; two versions of the same instructions is a contradiction.
func TestReinstallReplacesAnOlderBlockInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	old := "intro\n\n" + agentStart + "\nold words\n" + agentEnd + "\n\noutro\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgentBlock(path); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	if strings.Contains(got, "old words") {
		t.Error("the old block survived")
	}
	if !strings.HasPrefix(got, "intro\n\n") || !strings.HasSuffix(got, "\n\noutro\n") {
		t.Errorf("text around the block was disturbed:\n%s", got)
	}
	if strings.Count(got, agentStart) != 1 {
		t.Error("more than one block")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

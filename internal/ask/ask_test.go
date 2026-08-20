package ask

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
)

func TestPromptIsSelfContained(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	req := Request{
		ID: "20260819-120000.000", SessionID: "s1", Symbol: "app/cache.py::Cache.get",
		Question: "why does this return None instead of raising?", CreatedAt: time.Now(),
	}
	if err := s.Write(req, "## Source\n```\ndef get(self, key): ...\n```\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.PromptPath(req.ID))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(data)
	for _, want := range []string{
		req.Question,
		string(req.Symbol),
		".plum/ask/20260819-120000.000.answer.md", // where to write
		"def get(self, key)",                      // the assembled context travels with it
		"change no source files",                  // plum offers the developer the choice
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestPollAndWait(t *testing.T) {
	s := NewStore(t.TempDir())
	req := Request{ID: "q1", Symbol: "a.go::F", Question: "why?", CreatedAt: time.Now()}
	if err := s.Write(req, "context"); err != nil {
		t.Fatal(err)
	}
	if got := s.Poll("q1"); got.Status != "pending" {
		t.Fatalf("status = %q before an answer exists", got.Status)
	}
	if pending := s.Pending(); len(pending) != 1 || pending[0].ID != "q1" {
		t.Errorf("pending = %+v", pending)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(s.AnswerPath("q1"), []byte("  because the caller treats absence as normal.  "), 0o644)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := s.Wait(ctx, "q1", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "because the caller treats absence as normal." {
		t.Errorf("answer = %q", got.Text)
	}
	if len(s.Pending()) != 0 {
		t.Error("an answered question is no longer pending")
	}
}

// An answer nobody keeps is a chat message. These are the three things a kept
// answer can become.
func TestKeepWritesRationaleClaimAndPatch(t *testing.T) {
	root := t.TempDir()
	src := "package auth\n\nimport \"fmt\"\n\nfunc Get(key string) string {\n\treturn key\n}\n"
	if err := os.WriteFile(filepath.Join(root, "cache.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	sym := bundle.Symbol{
		ID: "cache.go::Get", Name: "Get", File: "cache.go",
		LineStart: 5, LineEnd: 7, Fingerprint: "sha256:abc",
	}
	req := Request{ID: "q1", Symbol: sym.ID, Question: "why no error?"}
	claimsPath := filepath.Join(root, "claims.yaml")

	res, err := Keep(root, ".plum/journal", claimsPath, req,
		"Absence is normal here, so the caller treats an empty string as a miss.\n\nA second paragraph that should not travel into the comment.",
		sym, Enrichment{Journal: true, Claim: true, Comment: true})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(root, ".plum", "journal"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no journal entry written: %v", err)
	}
	cs, err := claims.Load(claimsPath)
	if err != nil || len(cs) != 1 {
		t.Fatalf("claims = %v (%v)", cs, err)
	}
	if cs[0].Fingerprint != "sha256:abc" {
		t.Error("a kept claim must capture the subject's fingerprint, or it can never go stale")
	}
	if cs[0].Executable {
		t.Error("an answer is a trust-me assertion until someone writes a test for it")
	}

	patch, err := os.ReadFile(filepath.Join(root, res.PatchPath))
	if err != nil {
		t.Fatal(err)
	}
	p := string(patch)
	if !strings.HasPrefix(p, "--- a/cache.go\n+++ b/cache.go\n@@ ") {
		t.Errorf("not a unified diff:\n%s", p)
	}
	if !strings.Contains(p, "+// Absence is normal here") {
		t.Errorf("the comment is missing or not in Go syntax:\n%s", p)
	}
	if strings.Contains(p, "second paragraph") {
		t.Error("only the first paragraph belongs in a source comment")
	}
	// Nothing was applied: the source is untouched.
	after, _ := os.ReadFile(filepath.Join(root, "cache.go"))
	if string(after) != src {
		t.Error("Keep edited the source in place; it must only ever propose a patch")
	}
}

func TestCommentPatchUsesTheRightCommentSyntax(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cache.py"), []byte("import os\n\n\ndef get(key):\n    return key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sym := bundle.Symbol{ID: "cache.py::get", File: "cache.py", LineStart: 4, LineEnd: 5}
	patch, err := CommentPatch(root, sym, "Absence is normal here.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "+# Absence is normal here.") {
		t.Errorf("expected a Python comment:\n%s", patch)
	}
}

func TestKeepRefusesAnEmptyAnswer(t *testing.T) {
	if _, err := Keep(t.TempDir(), ".plum/journal", "claims.yaml", Request{}, "   ", bundle.Symbol{}, Enrichment{Journal: true}); err == nil {
		t.Error("keeping nothing should fail loudly")
	}
}

// This is the shape a real Claude Code answer arrives in: a heading, the
// question restated in bold, then the actual answer. Keeping the question as
// the answer produces a claim that asserts nothing.
const realReply = "# Answer — 20260819-234247.894\n\n" +
	"**Question:** what does `Cache.decorate` return when the key was missing, and is that intentional?\n\n" +
	"## What it returns\n\n" +
	"When the key is missing, `decorate` returns the string `'None@prod'`.\n\n" +
	"This is directly recorded, not inferred.\n"

func TestFirstParagraphSkipsHeadingsAndRestatedQuestions(t *testing.T) {
	got := firstParagraph(realReply)
	want := "When the key is missing, decorate returns the string 'None@prod'."
	if got != want {
		t.Errorf("kept the wrong text.\n got: %q\nwant: %q", got, want)
	}
}

func TestFirstParagraphHandlesPlainReplies(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Because absence is normal here.", "Because absence is normal here."},
		{"> why does this return nil?\n\nBecause absence is normal.", "Because absence is normal."},
		{"**Answer:** it returns nil.", "it returns nil."},
		{"---\n\nIt returns nil.", "It returns nil."},
		{"Wrapped across\ntwo lines.", "Wrapped across two lines."},
	} {
		if got := firstParagraph(tc.in); got != tc.want {
			t.Errorf("firstParagraph(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

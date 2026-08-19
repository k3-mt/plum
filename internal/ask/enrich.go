package ask

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/claims"
	"github.com/kelalaike/plum/internal/journal"
)

// Enrichment is what a kept answer becomes. An answer nobody keeps is a chat
// message; an answer kept here is rationale that survives into every future
// report, a claim that goes stale when the code moves, or a comment sitting
// where the next reader will actually look.
type Enrichment struct {
	Journal bool // record as rationale against the symbol (P3)
	Claim   bool // record as a claim, fingerprinted so it can go stale (P5)
	Comment bool // propose a source comment as a reviewable patch
}

type Result struct {
	JournalPath string   `json:"journal_path,omitempty"`
	ClaimID     string   `json:"claim_id,omitempty"`
	PatchPath   string   `json:"patch_path,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

// Keep applies the chosen enrichments. Source is never edited in place: the
// comment option writes a patch for review, because a tool that silently
// rewrites your code is a tool you stop trusting.
func Keep(root, journalDir, claimsPath string, req Request, answer string, sym bundle.Symbol, e Enrichment) (*Result, error) {
	res := &Result{}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, fmt.Errorf("there is no answer to keep")
	}

	if e.Journal {
		entry := bundle.JournalEntry{
			TS:        time.Now().UTC(),
			Tool:      "plum-ask",
			File:      sym.File,
			Rationale: fmt.Sprintf("%s — %s", req.Question, firstParagraph(answer)),
		}
		if err := journal.Append(filepath.Join(root, journalDir), entry); err != nil {
			return nil, err
		}
		res.JournalPath = journalDir
		res.Notes = append(res.Notes, "recorded as rationale; it will appear in every future report for this file")
	}

	if e.Claim {
		existing, _ := claims.Load(claimsPath)
		c := claims.Claim{
			ID:     nextClaimID(existing),
			Claim:  firstParagraph(answer),
			Symbol: sym.ID,
			// Captured at write time: when this symbol's fingerprint moves, the
			// claim is automatically suspect.
			Fingerprint: sym.Fingerprint,
			Executable:  false,
		}
		if err := claims.Save(claimsPath, append(existing, c)); err != nil {
			return nil, err
		}
		res.ClaimID = c.ID
		res.Notes = append(res.Notes, "recorded as a trust-me assertion; write a test body for it to make it executable")
	}

	if e.Comment {
		patch, err := CommentPatch(root, sym, answer)
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(root, ".plum", "patches")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, req.ID+".diff")
		if err := os.WriteFile(path, []byte(patch), 0o644); err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(root, path)
		res.PatchPath = rel
		res.Notes = append(res.Notes, "patch written; review it, then apply with: git apply "+rel)
	}
	return res, nil
}

// CommentPatch builds a unified diff that inserts the answer as a comment
// directly above the declaration, in the comment syntax of that file's language.
func CommentPatch(root string, sym bundle.Symbol, answer string) (string, error) {
	if sym.File == "" || sym.LineStart < 1 {
		return "", fmt.Errorf("this answer is not about a symbol with a source location")
	}
	data, err := os.ReadFile(filepath.Join(root, sym.File))
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if sym.LineStart > len(lines) {
		return "", fmt.Errorf("%s has moved since this session was captured — re-run plum before keeping this", sym.File)
	}

	marker := commentMarker(sym.File)
	indent := leadingSpace(lines[sym.LineStart-1])
	var added []string
	for _, line := range wrap(firstParagraph(answer), 72) {
		added = append(added, strings.TrimRight(indent+marker+" "+line, " "))
	}

	const contextLines = 3
	start := sym.LineStart - 1 - contextLines
	if start < 0 {
		start = 0
	}
	end := sym.LineStart - 1 + contextLines
	if end > len(lines) {
		end = len(lines)
	}

	var w strings.Builder
	fmt.Fprintf(&w, "--- a/%s\n", sym.File)
	fmt.Fprintf(&w, "+++ b/%s\n", sym.File)
	fmt.Fprintf(&w, "@@ -%d,%d +%d,%d @@\n", start+1, end-start, start+1, end-start+len(added))
	for i := start; i < sym.LineStart-1; i++ {
		w.WriteString(" " + lines[i] + "\n")
	}
	for _, a := range added {
		w.WriteString("+" + a + "\n")
	}
	for i := sym.LineStart - 1; i < end; i++ {
		w.WriteString(" " + lines[i] + "\n")
	}
	return w.String(), nil
}

func commentMarker(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".pyi", ".rb", ".sh", ".yaml", ".yml", ".toml", ".env", ".ini", ".cfg":
		return "#"
	case ".sql":
		return "--"
	case ".lua":
		return "--"
	default:
		return "//"
	}
}

func leadingSpace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// firstParagraph reduces a reply to the sentence that actually answers, for a
// claim or a source comment.
//
// Real agent replies open with a heading, often restate the question, and
// sometimes quote it back. All of that has to be skipped: keeping the question
// as the "answer" produces a claim that asserts nothing and a comment that
// tells the next reader what was asked rather than what is true.
func firstParagraph(s string) string {
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" || isScaffolding(para) {
			continue
		}
		return cleanMarkdown(para)
	}
	return cleanMarkdown(strings.TrimSpace(s))
}

// isScaffolding recognises the parts of a reply that frame the answer rather
// than being it: headings, quoted questions, restated prompts, rules.
func isScaffolding(para string) bool {
	first := strings.TrimSpace(strings.SplitN(para, "\n", 2)[0])
	switch {
	case strings.HasPrefix(first, "#"), strings.HasPrefix(first, ">"):
		return true
	case strings.HasPrefix(first, "---"), strings.HasPrefix(first, "==="), strings.HasPrefix(first, "***"):
		return true
	}
	lower := strings.ToLower(stripEmphasis(first))
	for _, label := range []string{"question:", "q:", "asked:", "prompt:", "context:"} {
		if strings.HasPrefix(lower, label) {
			return true
		}
	}
	return false
}

// cleanMarkdown unwraps a paragraph and drops the markup that means nothing in
// a source comment or a claims file.
func cleanMarkdown(para string) string {
	para = stripEmphasis(para)
	para = strings.ReplaceAll(para, "`", "")
	// A leading "Answer:" label is scaffolding too, once we are inside the text.
	for _, label := range []string{"Answer:", "answer:", "A:"} {
		if strings.HasPrefix(para, label) {
			para = strings.TrimSpace(strings.TrimPrefix(para, label))
		}
	}
	return strings.Join(strings.Fields(para), " ")
}

func stripEmphasis(s string) string {
	for _, marker := range []string{"**", "__"} {
		s = strings.ReplaceAll(s, marker, "")
	}
	return s
}

func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			out = append(out, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}

func nextClaimID(existing []claims.Claim) string {
	max := 0
	for _, c := range existing {
		var n int
		if _, err := fmt.Sscanf(c.ID, "c-%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("c-%03d", max+1)
}

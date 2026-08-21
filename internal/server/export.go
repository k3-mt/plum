package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/trace"
)

// Export writes the whole session as one HTML file that needs nothing running.
//
// The explore UI is a server because it answers questions and watches the tree.
// Reading is not that: reading happens in a pull request, in a chat, on a laptop
// six months later, and none of those can run a binary from your machine. So the
// same page is written out with the data folded in — every symbol's brief, the
// picture, the narration, the findings — and it opens from a file:// URL with no
// network of any kind.
//
// Nothing is regenerated for the export. It is the identical markup, stylesheet
// and scripts the server serves, so the artifact cannot drift from the tool.
func (s *Server) Export() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	payload := landscapePayload{
		AskRoute:       "offline",
		Summary:        trace.Summary(s.Landscape, s.Bundle),
		Narration:      trace.Narrate(s.Landscape, s.Bundle),
		Interpretation: s.interpretation(),
		Session:        s.Bundle.Session, Landscape: s.Landscape, Flow: s.Flow,
		Gate: s.Bundle.Gate, Synthesis: s.Synthesis, Claims: s.Claims,
		Symbols: s.Bundle.Symbols,
		Notes:   s.Landscape.Notes(), Unannotated: s.Landscape.UnannotatedExpensive(),
	}

	// Every symbol the page can reach has to travel with it: the frames on the
	// picture, and the joins and neighbours a click can follow off it. A brief
	// that is a fetch away is a broken link once the server stops.
	briefs := map[bundle.SymbolID]PromptContext{}
	add := func(id bundle.SymbolID) {
		if id == "" {
			return
		}
		if _, done := briefs[id]; done {
			return
		}
		pc := s.buildContext(id)
		pc.Markdown = renderContext(pc)
		briefs[id] = pc
	}
	for _, w := range s.Landscape.Wells {
		add(w.Symbol)
		for _, j := range w.Joins {
			add(j.Symbol)
		}
	}
	if s.Flow != nil {
		for _, n := range s.Flow.Nodes {
			add(n.Symbol)
		}
	}
	for _, sym := range s.Bundle.Symbols {
		add(sym.ID)
	}

	data, err := json.Marshal(struct {
		Payload landscapePayload                  `json:"payload"`
		Briefs  map[bundle.SymbolID]PromptContext `json:"briefs"`
	}{payload, briefs})
	if err != nil {
		return nil, err
	}

	page, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		return nil, err
	}
	html := string(page)

	// Inline the stylesheet and every script, in the order the page loads them.
	css, err := fs.ReadFile(assets, "assets/app.css")
	if err != nil {
		return nil, err
	}
	html = strings.Replace(html, `<link rel="stylesheet" href="/app.css">`,
		"<style>\n"+string(css)+"\n</style>", 1)

	// The install manifest and its icons describe an application you can run.
	// An export is a file you can open — there is nothing to install and no
	// server to fetch them from, so they come out rather than becoming three
	// dead links in an artifact whose whole promise is that it needs nothing.
	for _, tag := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<link rel="icon" href="/icon-192.png">`,
	} {
		html = strings.Replace(html, tag+"\n", "", 1)
	}

	for _, name := range []string{"code.js", "view.js", "flow.js", "landscape.js"} {
		src, err := fs.ReadFile(assets, "assets/"+name)
		if err != nil {
			return nil, err
		}
		html = strings.Replace(html, `<script src="/`+name+`"></script>`,
			"<script>\n"+guardScript(string(src))+"\n</script>", 1)
	}

	// The data goes in before the scripts run, so boot() finds it rather than
	// reaching for an endpoint that is not there.
	inject := "<script>window.__PLUM__ = " + string(data) + ";</script>\n"
	var out bytes.Buffer
	i := strings.Index(html, "<script>")
	if i < 0 {
		return nil, fmt.Errorf("the page has no scripts to inline into")
	}
	out.WriteString(html[:i])
	out.WriteString(inject)
	out.WriteString(html[i:])
	return out.Bytes(), nil
}

// guardScript keeps a `</script>` inside a string literal from ending the block
// it is inlined into. Nothing in these files has one today, which is exactly
// when this is worth writing down.
func guardScript(s string) string {
	return strings.ReplaceAll(s, "</script>", `<\/script>`)
}

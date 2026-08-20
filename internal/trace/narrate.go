package trace

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
)

// Step is one moment of the recording, said in plain language.
//
// A landscape names symbols; it does not say what happened. "Cache.Get →
// Cache.lookup → Cache.decorate" tells you the identity of each frame and
// nothing about the run. Every sentence here is composed from recorded
// evidence — the arguments that went in, the values that came back, the comment
// above the call, the cost of the transition — and where the evidence is
// missing the sentence says so rather than filling the gap.
//
// No model is involved. This is prose over verified structure (P1), which is
// why it is always available and always true.
type Step struct {
	Index int    `json:"index"`          // index into Wells, or -1 for a transition
	Kind  string `json:"kind"`           // frame | transition
	Text  string `json:"text"`           // the sentence, plain
	Note  string `json:"note,omitempty"` // what the evidence could not say
	// Spans is the same sentence with each part labelled by what it is. The
	// difference between a function name, a value the run actually produced,
	// and prose somebody wrote is meaningful, and it is lost the moment the
	// sentence is flattened to a string. It is decided here, where it is known,
	// rather than guessed at by a regex in the browser.
	Spans []Span `json:"spans,omitempty"`
}

// Span is one labelled part of a sentence.
type Span struct {
	// Kind is one of:
	//   text    ordinary prose written by this tool
	//   code    an identifier from the codebase — a function, a symbol
	//   value   something the run actually produced: an argument, a return
	//   quote   prose a person wrote: a doc comment, a call-site rationale
	//   cost    a measured duration
	//   risk    a mechanical finding
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// sentence accumulates labelled spans and can flatten to plain text.
type sentence struct{ spans []Span }

func (s *sentence) add(kind, text string) {
	if text == "" {
		return
	}
	// Runs of the same kind merge, so the browser renders fewer elements.
	if n := len(s.spans); n > 0 && s.spans[n-1].Kind == kind {
		s.spans[n-1].Text += text
		return
	}
	s.spans = append(s.spans, Span{Kind: kind, Text: text})
}

func (s *sentence) text(format string, a ...any) { s.add("text", fmt.Sprintf(format, a...)) }
func (s *sentence) code(v string)                { s.add("code", v) }
func (s *sentence) value(v string)               { s.add("value", v) }
func (s *sentence) quote(v string)               { s.add("quote", v) }
func (s *sentence) cost(v string)                { s.add("cost", v) }
func (s *sentence) risk(v string)                { s.add("risk", v) }

func (s *sentence) String() string {
	var b strings.Builder
	for _, sp := range s.spans {
		b.WriteString(sp.Text)
	}
	return b.String()
}

// Narrate walks the landscape and says what happened, one step at a time.
func Narrate(l Landscape, b *bundle.Bundle) []Step {
	var steps []Step
	barrierTo := map[int]Barrier{}
	for _, bar := range l.Barriers {
		barrierTo[bar.ToIdx] = bar
	}

	for i, w := range l.Wells {
		if bar, ok := barrierTo[i]; ok {
			steps = append(steps, transitionStep(l, b, bar))
		}
		steps = append(steps, frameStep(l, b, i, w))
	}
	return steps
}

func frameStep(l Landscape, b *bundle.Bundle, i int, w Well) Step {
	name := label(w)
	step := Step{Index: i, Kind: "frame"}
	var s sentence

	switch w.Phase {
	case "resume":
		s.text("Control came back into ")
		s.code(name)
		if from := returnedValue(w); from != "" {
			s.text(" with ")
			s.value(from)
		}
		s.text(".")
		step.Text, step.Spans = s.String(), s.spans
		return step
	case "escape":
		s.text("The failure left ")
		s.code(name)
		s.text(" without anything catching it.")
		step.Text, step.Spans = s.String(), s.spans
		step.Note = "nothing in the changed code handles this — the error boundary is outside what was traced"
		return step
	}

	s.code(name)
	if args := calledWith(w); args != "" {
		s.text(" was called with ")
		s.value(args)
		s.text(".")
	} else {
		s.text(" was called.")
	}
	if ret := returnedValue(w); ret != "" {
		s.text(" It returned ")
		s.value(ret)
		s.text(".")
	}

	sym := b.Lookup(w.Symbol)
	if doc := firstSentence(sym.Doc); doc != "" {
		s.text(" Its own description: ")
		s.quote(strconv.Quote(doc))
		s.text(".")
	} else if !w.Context {
		step.Note = "no description was written for this function, so what it is for is not recorded anywhere"
	}
	if w.Context {
		step.Note = "this session did not change this function; it is here because the run passed through it"
	}
	if risks := b.RisksFor(w.Symbol); len(risks) > 0 {
		s.text(" Flagged: ")
		s.risk(risks[0].Note)
		s.text(".")
	}
	step.Text, step.Spans = s.String(), s.spans
	return step
}

func transitionStep(l Landscape, b *bundle.Bundle, bar Barrier) Step {
	from, to := label(l.Wells[bar.FromIdx]), label(l.Wells[bar.ToIdx])
	step := Step{Index: -1, Kind: "transition"}
	var s sentence

	switch bar.Direction {
	case "descend":
		s.code(from)
		s.text(" then called ")
		s.code(to)
		s.text(", which took ")
		s.cost(humanDuration(bar.CostNanos))
		s.text(".")
		if bar.Rationale != "" {
			s.text(" The code says why: ")
			s.quote(strconv.Quote(oneLine(bar.Rationale)))
			s.text(".")
		} else if bar.Height >= 0.6 {
			step.Note = "this is one of the more expensive steps on the path, and nothing at the call site explains why it is made"
		}
	case "ascend":
		s.code(from)
		s.text(" finished and handed control back to ")
		s.code(to)
		s.text(", ")
		s.cost(humanDuration(bar.CostNanos))
		s.text(" later.")
	case "unwind":
		s.text("A failure in ")
		s.code(from)
		s.text(" unwound ")
		s.risk(plural(bar.Frames, "frame"))
		s.text(" at once and landed in ")
		s.code(to)
		s.text(", ")
		s.cost(humanDuration(bar.CostNanos))
		s.text(" later.")
		if bar.Frames > 1 {
			step.Note = "an exception skips past every frame in between — none of them get to finish"
		}
	}
	if bar.Kind == "network" || bar.Kind == "io" {
		s.text(" That step looks like ")
		s.risk(bar.Kind)
		s.text(" work rather than computation.")
	}
	step.Text, step.Spans = s.String(), s.spans
	return step
}

// Summary is the whole recording as one short paragraph, for a header or a
// prompt — the answer to "what does this actually do?".
func Summary(l Landscape, b *bundle.Bundle) string {
	if len(l.Wells) == 0 {
		return "Nothing was recorded: no traced test entered the changed code."
	}
	entered := make([]string, 0, len(l.Wells))
	seen := map[bundle.SymbolID]bool{}
	changed, context := 0, 0
	for _, w := range l.Wells {
		if w.Phase != "enter" || seen[w.Symbol] {
			continue
		}
		seen[w.Symbol] = true
		entered = append(entered, label(w))
		if w.Context {
			context++
		} else {
			changed++
		}
	}

	var s strings.Builder
	if l.TestID != "" {
		fmt.Fprintf(&s, "Running %q, ", l.TestID)
	} else {
		s.WriteString("On the busiest path recorded, ")
	}
	fmt.Fprintf(&s, "the run went through %s: %s.", plural(len(entered), unitOf(l, b)), strings.Join(entered, " → "))
	if context > 0 {
		fmt.Fprintf(&s, " %d of those this session changed; %d it merely passed through.", changed, context)
	}
	var failures []string
	for _, bar := range l.Barriers {
		if bar.Direction == "unwind" && bar.FromIdx < len(l.Wells) {
			failures = append(failures, label(l.Wells[bar.FromIdx]))
		}
	}
	switch {
	case l.Escaped != "":
		fmt.Fprintf(&s, " It ended in a failure that nothing caught: %s.", oneLine(l.Escaped))
	case !l.Closed:
		fmt.Fprintf(&s, " One frame never returned: %s.", l.OpenFrame)
	case len(failures) > 0:
		// Everything came back, but something in it failed — saying only the
		// former would read as an all-clear.
		fmt.Fprintf(&s, " Everything entered came back, but %s failed: %s.",
			plural(len(failures), "step"), strings.Join(failures, ", "))
	default:
		s.WriteString(" Every frame that was entered came back.")
	}
	return s.String()
}

// unitOf names what was entered in the vocabulary of what is being read. A dbt
// build goes through models, not functions, and calling them functions is the
// kind of small wrongness that makes a reader stop trusting the rest.
func unitOf(l Landscape, b *bundle.Bundle) string {
	counts := map[string]int{}
	for _, w := range l.Wells {
		if w.Phase == "enter" {
			counts[b.Lookup(w.Symbol).Kind]++
		}
	}
	switch {
	case counts["model"] > 0:
		return "node"
	case counts["column"] > 0 || counts["config_key"] > 0:
		return "declaration"
	}
	return "function"
}

// ---------------------------------------------------------------- helpers

func label(w Well) string {
	if w.Label != "" {
		return w.Label
	}
	return string(w.Symbol)
}

// calledWith renders the recorded arguments the way a person would read them.
func calledWith(w Well) string {
	for _, inv := range w.Invocations {
		if strings.HasPrefix(inv, "→") || strings.HasPrefix(inv, "←") || strings.HasPrefix(inv, "✗") {
			continue
		}
		if strings.TrimSpace(inv) == "" {
			continue
		}
		return humanArgs(inv)
	}
	return ""
}

// HumanArgs renders a recorded argument map the way a person reads it.
func HumanArgs(args map[string]string) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s = %s", k, HumanValue(args[k])))
	}
	return joinWithAnd(parts)
}

// HumanValue names the empty cases rather than printing a token nobody outside
// the language reads as "absent".
func HumanValue(v string) string { return humanValue(v) }

// StepsFor returns the narration sentences that mention one symbol, so a brief
// about that symbol can say what it actually did.
func StepsFor(l Landscape, b *bundle.Bundle, sym bundle.SymbolID) []Step {
	var out []Step
	steps := Narrate(l, b)
	for i, s := range steps {
		if s.Kind != "frame" || s.Index < 0 || s.Index >= len(l.Wells) {
			continue
		}
		if l.Wells[s.Index].Symbol != sym {
			continue
		}
		// The transition that led here explains why it was entered.
		if i > 0 && steps[i-1].Kind == "transition" {
			out = append(out, steps[i-1])
		}
		out = append(out, s)
	}
	return out
}

// humanArgs turns `key=user:42, opts=<nil>` into `key = "user:42" and opts = nothing`.
func humanArgs(raw string) string {
	parts := strings.Split(raw, ", ")
	var out []string
	for _, p := range parts {
		name, value, ok := strings.Cut(p, "=")
		if !ok {
			out = append(out, p)
			continue
		}
		out = append(out, fmt.Sprintf("%s = %s", strings.TrimSpace(name), humanValue(value)))
	}
	return joinWithAnd(out)
}

func returnedValue(w Well) string {
	for _, inv := range w.Invocations {
		switch {
		case strings.HasPrefix(inv, "→ "):
			return humanValue(strings.TrimPrefix(inv, "→ "))
		case strings.HasPrefix(inv, "← "):
			// A resumed frame records which frame handed back, and what.
			rest := strings.TrimPrefix(inv, "← ")
			if name, value, ok := strings.Cut(rest, " "); ok && strings.TrimSpace(value) != "" {
				return fmt.Sprintf("%s from %s", humanValue(value), name)
			}
			return ""
		case strings.HasPrefix(inv, "✗ "):
			return humanValue(strings.TrimPrefix(inv, "✗ "))
		}
	}
	return ""
}

// humanValue names the empty cases rather than printing a token nobody outside
// the language reads as "absent".
func humanValue(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "", "<nil>", "nil", "None", "undefined", "null":
		return "nothing"
	}
	if strings.HasSuffix(v, ", <nil>") {
		return fmt.Sprintf("%q and no error", strings.TrimSuffix(v, ", <nil>"))
	}
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'") {
		return v
	}
	return fmt.Sprintf("%q", v)
}

func firstSentence(doc string) string {
	doc = oneLine(doc)
	if i := strings.Index(doc, ". "); i >= 0 {
		return doc[:i+1]
	}
	return doc
}

func humanDuration(ns int64) string {
	d := time.Duration(ns)
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", ns)
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

func joinWithAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

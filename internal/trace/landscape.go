package trace

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
)

// Well is one frame on the reaction coordinate. Vertical is stack depth.
type Well struct {
	Symbol      bundle.SymbolID `json:"symbol"`
	Label       string          `json:"label"`
	Depth       int             `json:"depth"`
	SelfNanos   int64           `json:"self_ns"` // width
	Phase       string          `json:"phase"`   // enter | resume  ← resume renders faded
	Doc         string          `json:"doc"`     // "" ⇒ rendered dashed
	Risk        bool            `json:"risk"`
	Invocations []string        `json:"invocations"`
}

// Barrier is the transition between two wells. Descent is entering a call,
// ascent is returning, and an unwind spanning several frames is a cliff.
type Barrier struct {
	FromIdx   int     `json:"from"`
	ToIdx     int     `json:"to"`
	Direction string  `json:"direction"` // descend | ascend | unwind
	CostNanos int64   `json:"cost_ns"`
	Height    float64 `json:"height"` // log-normalised 0..1
	Kind      string  `json:"kind"`   // compute | io | lock | network | raise
	Rationale string  `json:"rationale"`
	Frames    int     `json:"frames"` // >1 on unwind ⇒ rendered as a cliff
}

type Landscape struct {
	SessionID string    `json:"session_id"`
	Wells     []Well    `json:"wells"`
	Barriers  []Barrier `json:"barriers"`
	// Closed is false when a frame descended and never came back: a goroutine
	// leak, a swallowed context, an early exit. You see it as a shape.
	Closed    bool   `json:"closed"`
	OpenFrame string `json:"open_frame"`
	TestID    string `json:"test_id"`
	Chain     string `json:"chain"`
	// Escaped is the panic that left the instrumented set entirely — nothing in
	// the changed code caught it.
	Escaped string `json:"escaped,omitempty"`
	// Truncated counts frames beyond the render budget. Reported, never hidden.
	Truncated int      `json:"truncated,omitempty"`
	Chains    int      `json:"chains"`
	HotPath   []string `json:"hot_path"`
}

// Chain selects which invocation tree the landscape renders. A test suite that
// exercises two very different paths is exactly when this stops being academic
// (spec §14.3).
type Chain string

const (
	ChainHottest Chain = "hottest" // most frames — the path most of the work went through
	ChainSlowest Chain = "slowest" // longest wall-clock span
	ChainRaising Chain = "raising" // the one that raised, if any
)

// Derive walks call AND return events, producing a path that descends into
// nested frames and ascends as they complete, closing at the entry depth (§9.3).
func Derive(events []Event, b *bundle.Bundle) Landscape {
	return DeriveChain(events, b, ChainHottest)
}

// DefaultMaxFrames bounds how much of a chain is rendered. A real suite produces
// chains of a thousand frames, which is not a landscape anyone can read. The
// remainder is counted and reported, never silently dropped.
const DefaultMaxFrames = 80

// DeriveChain is Derive with an explicit representative-chain policy.
func DeriveChain(events []Event, b *bundle.Bundle, pick Chain) Landscape {
	return DeriveChainN(events, b, pick, DefaultMaxFrames)
}

// DeriveChainN is DeriveChain with an explicit frame budget; 0 means unbounded.
func DeriveChainN(events []Event, b *bundle.Bundle, pick Chain, maxFrames int) Landscape {
	l := deriveChain(events, b, pick)
	if maxFrames > 0 && len(l.Wells) > maxFrames {
		l.Truncated = len(l.Wells) - maxFrames
		l.Wells = l.Wells[:maxFrames]
		var kept []Barrier
		for _, bar := range l.Barriers {
			if bar.FromIdx < maxFrames && bar.ToIdx < maxFrames {
				kept = append(kept, bar)
			}
		}
		l.Barriers = kept
		var hot []string
		for _, w := range l.Wells {
			if w.Phase == "enter" {
				hot = append(hot, string(w.Symbol))
			}
		}
		l.HotPath = hot
	}
	return l
}

func deriveChain(events []Event, b *bundle.Bundle, pick Chain) Landscape {
	chain, chains := representativeChain(events, pick)
	l := Landscape{SessionID: b.Session.ID, Chains: chains, Closed: true, Chain: string(pick)}
	if len(chain) == 0 {
		return l
	}
	l.TestID = chain[0].TestID

	minCost, maxCost := costRange(chain)

	var stack []int // indices into wells, innermost last
	openArgs := map[string]map[string]string{}

	emit := func(sym bundle.SymbolID, depth int, phase string, inv string) int {
		s := b.Lookup(sym)
		w := Well{
			Symbol: sym, Label: s.Name, Depth: depth, Phase: phase,
			Doc: s.Doc, Risk: b.HasRisk(sym),
		}
		if w.Label == "" {
			w.Label = string(sym)
		}
		if inv != "" {
			w.Invocations = []string{inv}
		}
		l.Wells = append(l.Wells, w)
		return len(l.Wells) - 1
	}

	link := func(dir string, cost int64, frames int, ev Event, parent bundle.SymbolID) {
		if len(l.Wells) < 2 {
			return
		}
		if cost < 0 {
			cost = 0
		}
		bar := Barrier{
			FromIdx:   len(l.Wells) - 2,
			ToIdx:     len(l.Wells) - 1,
			Direction: dir,
			CostNanos: cost,
			Frames:    frames,
			Height:    logNorm(cost, minCost, maxCost),
			Kind:      classify(b.Lookup(ev.Symbol), cost, dir),
		}
		// A call-site comment explains why the call was made, so it belongs on
		// the way in. Repeating it on the return would claim it explains the
		// cost of coming back, which it does not.
		if dir != "ascend" {
			bar.Rationale = b.CallSiteComment(parent, ev.Symbol)
		}
		l.Barriers = append(l.Barriers, bar)
	}

	for i := 0; i < len(chain); i++ {
		ev := chain[i]
		var prevTS int64
		if i > 0 {
			prevTS = chain[i-1].TSNanos
		}
		switch ev.Kind {
		case "call":
			var parent bundle.SymbolID
			if len(stack) > 0 {
				parent = l.Wells[stack[len(stack)-1]].Symbol
			}
			emit(ev.Symbol, ev.Depth, "enter", formatArgs(ev))
			openArgs[ev.InvocationID] = ev.Args
			if i > 0 {
				link("descend", ev.TSNanos-prevTS, 1, ev, parent)
			}
			stack = append(stack, len(l.Wells)-1)

		case "return":
			if len(stack) == 0 {
				continue
			}
			// Attribute the returned value to the frame that produced it.
			top := stack[len(stack)-1]
			leaving := l.Wells[top]
			l.Wells[top].Invocations = append(l.Wells[top].Invocations, "→ "+truncate(ev.Result, 200))
			l.Wells[top].SelfNanos = ev.TSNanos - prevTS
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				continue // returned past the entry point; the path is closed
			}
			parent := l.Wells[stack[len(stack)-1]]
			emit(parent.Symbol, parent.Depth, "resume", "← "+leaving.Label+" "+truncate(ev.Result, 120))
			link("ascend", ev.TSNanos-prevTS, 1, ev, parent.Symbol)

		case "raise":
			// One panic passes through every frame's probe on its way out, so the
			// shim emits a run of raise events. They are one event in the world:
			// a panic unwinding four frames drops straight from depth 4 to depth 0,
			// a cliff rather than a staircase (§9.3).
			last := i
			for last+1 < len(chain) && chain[last+1].Kind == "raise" && chain[last+1].Exception == ev.Exception {
				last++
			}
			deepest := chain[last]
			target := unwindTarget(l.Wells, stack, deepest)
			frames := len(stack) - target
			if frames < 1 {
				frames = 1
			}
			if target < 0 {
				target = 0
			}
			stack = stack[:target]
			if len(stack) > 0 {
				parent := l.Wells[stack[len(stack)-1]]
				emit(parent.Symbol, parent.Depth, "resume", "✗ "+truncate(ev.Exception, 200))
				link("unwind", chain[last].TSNanos-prevTS, frames, ev, parent.Symbol)
			} else {
				// Nothing inside the traced set caught it. The cliff still happened
				// and is still the most legible thing on the landscape, so it is
				// drawn falling to depth 0 and marked as an escape.
				l.Escaped = truncate(ev.Exception, 200)
				emit(deepest.Symbol, 0, "escape", "✗ escaped: "+truncate(ev.Exception, 200))
				link("unwind", chain[last].TSNanos-prevTS, frames, ev, deepest.Symbol)
			}
			i = last
		}
	}

	if len(stack) > 0 {
		l.Closed = false
		l.OpenFrame = string(l.Wells[stack[len(stack)-1]].Symbol)
	}
	for _, w := range l.Wells {
		if w.Phase == "enter" {
			l.HotPath = append(l.HotPath, string(w.Symbol))
		}
	}
	return l
}

// representativeChain picks one invocation tree to render: the hottest path, or
// the slowest when several are equally hot (spec §14.3 — this is the knob).
func representativeChain(events []Event, pick Chain) ([]Event, int) {
	SortByTime(events)
	type chain struct {
		events []Event
		span   int64
		frames int
	}
	var chains []chain
	var cur *chain
	depth := 0
	for _, ev := range events {
		if cur == nil {
			if ev.Kind != "call" {
				continue
			}
			chains = append(chains, chain{})
			cur = &chains[len(chains)-1]
			depth = 0
		}
		cur.events = append(cur.events, ev)
		switch ev.Kind {
		case "call":
			depth++
			cur.frames++
		case "return", "raise":
			// Each frame's probe reports its own exit, including on the way out of
			// a panic, so a raise closes exactly one frame here. Coalescing the run
			// into a single cliff is Derive's job, not the chain splitter's.
			depth--
		}
		if depth <= 0 {
			cur.span = cur.events[len(cur.events)-1].TSNanos - cur.events[0].TSNanos
			cur = nil
		}
	}
	if cur != nil { // an unclosed chain is still evidence — that is the point
		cur.span = cur.events[len(cur.events)-1].TSNanos - cur.events[0].TSNanos
	}
	if len(chains) == 0 {
		return nil, 0
	}
	best := 0
	for i, c := range chains {
		bc := chains[best]
		switch pick {
		case ChainSlowest:
			if c.span > bc.span {
				best = i
			}
		case ChainRaising:
			// Prefer a chain that actually raised; fall back to the hottest.
			if raises(c.events) && (!raises(bc.events) || c.frames > bc.frames) {
				best = i
			} else if !raises(bc.events) && c.frames > bc.frames {
				best = i
			}
		default:
			if c.frames > bc.frames || (c.frames == bc.frames && c.span > bc.span) {
				best = i
			}
		}
	}
	return chains[best].events, len(chains)
}

func raises(evs []Event) bool {
	for _, e := range evs {
		if e.Kind == "raise" {
			return true
		}
	}
	return false
}

func costRange(chain []Event) (int64, int64) {
	minC, maxC := int64(math.MaxInt64), int64(0)
	for i := 1; i < len(chain); i++ {
		d := chain[i].TSNanos - chain[i-1].TSNanos
		if d < 0 {
			d = 0
		}
		if d < minC {
			minC = d
		}
		if d > maxC {
			maxC = d
		}
	}
	if minC == int64(math.MaxInt64) {
		minC = 0
	}
	return minC, maxC
}

// logNorm scales barrier height logarithmically: a 340ms network hop must dwarf
// a 2µs call without a 100µs call vanishing entirely (§9.3).
func logNorm(cost, minCost, maxCost int64) float64 {
	if maxCost <= minCost {
		if cost > 0 {
			return 0.5
		}
		return 0.1
	}
	lo := math.Log1p(float64(minCost))
	hi := math.Log1p(float64(maxCost))
	v := (math.Log1p(float64(cost)) - lo) / (hi - lo)
	return math.Round(math.Max(0.05, math.Min(1, v))*1000) / 1000
}

// classify names what the transition cost was spent on, using the risk markers
// already attached to the symbol plus the observed cost.
func classify(s bundle.Symbol, cost int64, dir string) string {
	if dir == "unwind" {
		return "raise"
	}
	name := s.Name + " " + s.Signature
	switch {
	case containsAny(name, "http", "HTTP", "Fetch", "Request", "Dial", "Client"):
		return "network"
	case containsAny(name, "Read", "Write", "Open", "File", "Load", "Save", "Flush"):
		return "io"
	case containsAny(name, "Lock", "Mutex", "Wait", "Acquire"):
		return "lock"
	case cost > int64(10*time.Millisecond):
		return "io" // this long without a hint is almost never pure compute
	}
	return "compute"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// unwindTarget finds the depth a raise unwinds to: the frame that catches it,
// or the base of the stack when nothing does.
// unwindTarget returns the stack depth the panic lands at: the frame below the
// deepest one it passed through, or the base when it escapes entirely.
func unwindTarget(wells []Well, stack []int, deepest Event) int {
	for i := len(stack) - 1; i >= 0; i-- {
		if wells[stack[i]].Symbol == deepest.Symbol {
			return i
		}
	}
	return 0
}

func formatArgs(ev Event) string {
	if len(ev.Args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ev.Args))
	for k := range ev.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += k + "=" + truncate(ev.Args[k], 200)
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Notes is what the report should say about this landscape.
func (l Landscape) Notes() []string {
	var out []string
	if len(l.Wells) == 0 {
		return nil
	}
	if l.Escaped != "" {
		out = append(out, fmt.Sprintf("a panic escaped the instrumented set entirely: `%s` — nothing in the changed code caught it", l.Escaped))
	}
	if !l.Closed {
		out = append(out, fmt.Sprintf("**the path does not close** — `%s` was entered and never returned during the test run", l.OpenFrame))
	}
	out = append(out, fmt.Sprintf("%d frames on the representative chain, %d chains recorded", len(l.Wells)+l.Truncated, l.Chains))
	if l.Truncated > 0 {
		out = append(out, fmt.Sprintf("showing the first %d frames; %d more are recorded in traces/ — raise the budget with `plum landscape -frames N`", len(l.Wells), l.Truncated))
	}
	for _, bar := range l.Barriers {
		if bar.Direction == "unwind" && bar.Frames > 1 {
			out = append(out, fmt.Sprintf("an exception unwound %d frames at once — the error boundary sits at `%s`", bar.Frames, l.Wells[bar.ToIdx].Symbol))
		}
	}
	var undocumented int
	for _, w := range l.Wells {
		if w.Phase == "enter" && w.Doc == "" {
			undocumented++
		}
	}
	if undocumented > 0 {
		out = append(out, fmt.Sprintf("%d frames on the hot path have no declaration doc", undocumented))
	}
	return out
}

// UnannotatedExpensive lists costly transitions whose call site carries no
// comment. An expensive barrier nobody explained is a first-class finding (§9.4).
func (l Landscape) UnannotatedExpensive() []string {
	var out []string
	for _, bar := range l.Barriers {
		if bar.Rationale != "" || bar.Height < 0.6 || bar.Direction == "ascend" {
			continue
		}
		out = append(out, fmt.Sprintf("`%s` → `%s` cost %s (%s) with no call-site comment",
			l.Wells[bar.FromIdx].Symbol, l.Wells[bar.ToIdx].Symbol,
			time.Duration(bar.CostNanos).Round(time.Microsecond), bar.Kind))
	}
	return out
}

func (l Landscape) Save(path string) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func LoadLandscape(path string) (*Landscape, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Landscape
	return &l, json.Unmarshal(data, &l)
}

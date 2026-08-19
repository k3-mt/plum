// Package bundle defines the contract between capture/extract and everything
// downstream. These types are versioned from day one (spec §4): the schema is
// the seam, and nothing downstream of bundle.json knows how a session was made.
package bundle

import "time"

// SchemaVersion is bumped whenever a field changes meaning. Readers must
// refuse bundles from a newer major version.
const SchemaVersion = "1.0"

type Bundle struct {
	SchemaVersion string         `json:"schema_version"`
	Session       Session        `json:"session"`
	Files         []FileChange   `json:"files"`
	Symbols       []Symbol       `json:"symbols"`
	Surface       SurfaceDelta   `json:"public_surface"`
	Edges         []Edge         `json:"edges"`
	Deps          DepDelta       `json:"dependencies"`
	RiskMarkers   []RiskMarker   `json:"risk_markers"`
	Divergence    Divergence     `json:"divergence"`
	Coverage      Coverage       `json:"coverage"`
	Journal       []JournalEntry `json:"journal"`
	Gate          Gate           `json:"gate"`
}

type Session struct {
	ID             string    `json:"id"`
	StartSHA       string    `json:"start_sha"`
	EndSHA         string    `json:"end_sha"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at"`
	Command        string    `json:"command"`
	Agent          string    `json:"agent"`
	JournalPresent bool      `json:"journal_present"`
	Repo           string    `json:"repo"`
}

// SymbolID is "<relpath>::<qualified name>" — the join key for the entire
// system. Traces, claims, synthesis chunks and explore telemetry all key on it.
type SymbolID string

type FileChange struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Change    string `json:"change"` // added | modified | deleted | renamed
	Added     int    `json:"added"`
	Deleted   int    `json:"deleted"`
	Language  string `json:"language"`
	Binary    bool   `json:"binary"`
	Migration bool   `json:"migration"`
}

type Symbol struct {
	ID          SymbolID   `json:"id"`
	Kind        string     `json:"kind"` // func | method | class | type | var | const
	Name        string     `json:"name"`
	File        string     `json:"file"`
	LineStart   int        `json:"line_start"`
	LineEnd     int        `json:"line_end"`
	ByteStart   int        `json:"byte_start"`
	ByteEnd     int        `json:"byte_end"`
	Change      string     `json:"change"` // added | modified | deleted
	Signature   string     `json:"signature"`
	Fingerprint string     `json:"fingerprint"` // sha256 of normalised subtree — drives P5
	Doc         string     `json:"doc"`
	Exported    bool       `json:"exported"`
	Comments    []Comment  `json:"comments"`
	CallSites   []CallSite `json:"call_sites"`
	Tested      bool       `json:"tested"`
}

type Comment struct {
	Text      string `json:"text"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// CallSite binds an outbound call to the comment block directly above it.
// This is what annotates barriers in the landscape (spec §9.4).
type CallSite struct {
	Callee    SymbolID `json:"callee"`
	CalleeRaw string   `json:"callee_raw"`
	Line      int      `json:"line"`
	Rationale string   `json:"rationale"` // "" ⇒ unannotated; flagged if expensive
}

type SurfaceDelta struct {
	Added    []SurfaceItem `json:"added"`
	Removed  []SurfaceItem `json:"removed"`
	Modified []SurfaceMod  `json:"modified"`
}

type SurfaceItem struct {
	Kind      string   `json:"kind"` // export | route | env_var | cli_flag | migration
	Name      string   `json:"name"`
	File      string   `json:"file"`
	Symbol    SymbolID `json:"symbol,omitempty"`
	Signature string   `json:"signature,omitempty"`
}

type SurfaceMod struct {
	SurfaceItem
	Before string `json:"before"`
	After  string `json:"after"`
}

type Edge struct {
	From          SymbolID `json:"from"`
	To            SymbolID `json:"to"`
	Kind          string   `json:"kind"` // call
	CrossesModule bool     `json:"crosses_module"`
	New           bool     `json:"new"`
}

type DepDelta struct {
	Added    []Dep    `json:"added"`
	Removed  []Dep    `json:"removed"`
	Upgraded []DepMod `json:"upgraded"`
}

type Dep struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

type DepMod struct {
	Dep
	Before string `json:"before"`
	After  string `json:"after"`
}

type RiskMarker struct {
	Kind   string   `json:"kind"`
	Symbol SymbolID `json:"symbol"`
	File   string   `json:"file"`
	Line   int      `json:"line"`
	Note   string   `json:"note"`
}

type Divergence struct {
	Score    float64             `json:"score"`
	Findings []DivergenceFinding `json:"findings"`
}

type DivergenceFinding struct {
	Convention string   `json:"convention"`
	Expected   string   `json:"expected"`
	Observed   string   `json:"observed"`
	Symbol     SymbolID `json:"symbol"`
	Severity   string   `json:"severity"` // info | warn | high
	Source     string   `json:"source"`   // config | empirical
}

type Coverage struct {
	TestCommand  string     `json:"test_command"`
	Untested     []SymbolID `json:"untested"`
	TestedCount  int        `json:"tested_count"`
	SymbolCount  int        `json:"symbol_count"`
	TestFilesNew []string   `json:"test_files_new"`
}

type JournalEntry struct {
	TS           time.Time `json:"ts"`
	Tool         string    `json:"tool"`
	File         string    `json:"file"`
	Rationale    string    `json:"rationale"`
	Alternatives []string  `json:"alternatives_considered"`
}

type Gate struct {
	Fired   bool     `json:"fired"`
	Reasons []string `json:"reasons"`
}

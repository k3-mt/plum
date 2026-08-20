package conf

import (
	"strings"
	"testing"

	"github.com/k3-mt/plum/internal/bundle"
)

const yamlSrc = `server:
  # how long we wait before giving up
  timeout: 30
  bind: 127.0.0.1
auth:
  AUTH_REALM: staging
  api_key: sk-live-abc123
  from_env: ${AUTH_TOKEN}
  verify_tls: true
logging:
  debug: true
features:
  - alpha
  - beta
`

func keyed(t *testing.T, path, src string) map[string]Key {
	t.Helper()
	m := map[string]Key{}
	for _, k := range Parse(path, []byte(src)) {
		m[k.Path] = k
	}
	return m
}

func TestYAMLKeyPathsAndComments(t *testing.T) {
	m := keyed(t, "app.yaml", yamlSrc)
	for path, want := range map[string]string{
		"server.timeout":  "30",
		"server.bind":     "127.0.0.1",
		"auth.AUTH_REALM": "staging",
		"logging.debug":   "true",
		"features.[0]":    "alpha",
		"features.[1]":    "beta",
	} {
		k, ok := m[path]
		if !ok {
			t.Fatalf("missing key %q (found %d keys)", path, len(m))
		}
		if k.Value != want {
			t.Errorf("%s = %q, want %q", path, k.Value, want)
		}
	}
	// The comment above a setting explains it, exactly as it does above a call.
	if got := m["server.timeout"].Comment; !strings.Contains(got, "how long we wait") {
		t.Errorf("comment = %q", got)
	}
	if m["server.timeout"].Line != 3 {
		t.Errorf("line = %d, want 3", m["server.timeout"].Line)
	}
}

func TestSecretsAreRedactedNotPrinted(t *testing.T) {
	a := New()
	syms, err := a.ParseSymbols("app.yaml", []byte(yamlSrc))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		if strings.Contains(s.Signature, "sk-live-abc123") {
			t.Fatalf("a secret value reached the bundle: %q", s.Signature)
		}
	}
	var found bool
	for _, s := range syms {
		if s.ID == bundle.SymbolID("app.yaml::auth.api_key") {
			found = true
			if !strings.Contains(s.Signature, "<redacted>") {
				t.Errorf("api_key signature = %q", s.Signature)
			}
		}
	}
	if !found {
		t.Error("the key itself should still be visible, only its value hidden")
	}
}

func TestRiskPredicates(t *testing.T) {
	risky := `server:
  bind: 0.0.0.0
  timeout: 0
auth:
  api_key: sk-live-abc123
  from_env: ${AUTH_TOKEN}
  verify_tls: false
logging:
  debug: true
`
	a := New()
	syms, _ := a.ParseSymbols("app.yaml", []byte(risky))
	marks, err := a.RiskMarkers("app.yaml", []byte(risky), syms)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, m := range marks {
		kinds[m.Kind] = string(m.Symbol)
	}
	for _, want := range []string{"wildcard_binding", "timeout_disabled", "hardcoded_secret", "verification_disabled", "debug_enabled"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("missing %q (got %v)", want, kinds)
		}
	}
	// A value that defers to the environment is the fix for a hardcoded secret,
	// not an instance of one.
	for _, m := range marks {
		if m.Kind == "hardcoded_secret" && strings.Contains(string(m.Symbol), "from_env") {
			t.Error("${AUTH_TOKEN} is a reference, not a hardcoded secret")
		}
	}
}

func TestFingerprintTracksTheValue(t *testing.T) {
	a := New()
	fp := func(src string) string {
		syms, err := a.ParseSymbols("app.yaml", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range syms {
			if s.Name == "server.timeout" {
				return s.Fingerprint
			}
		}
		return ""
	}
	base := "server:\n  timeout: 30\n"
	recommented := "server:\n  # a new comment\n  timeout: 30\n"
	changed := "server:\n  timeout: 0\n"

	if fp(base) != fp(recommented) {
		t.Error("a comment change must not invalidate claims about a setting")
	}
	if fp(base) == fp(changed) {
		t.Error("changing the value must move the fingerprint: that is the behaviour change")
	}
}

func TestTOMLAndEnvAndJSON(t *testing.T) {
	toml := keyed(t, "cfg.toml", "[server]\ntimeout = 30 # inline comment\nbind = \"127.0.0.1\"\n")
	if toml["server.timeout"].Value != "30" {
		t.Errorf("toml timeout = %q", toml["server.timeout"].Value)
	}
	if toml["server.bind"].Value != `"127.0.0.1"` {
		t.Errorf("toml bind = %q", toml["server.bind"].Value)
	}

	env := keyed(t, ".env", "# the realm\nexport AUTH_REALM=prod\nDEBUG=1\n")
	if env["AUTH_REALM"].Value != "prod" || env["DEBUG"].Value != "1" {
		t.Errorf("env = %+v", env)
	}
	if !strings.Contains(env["AUTH_REALM"].Comment, "the realm") {
		t.Errorf("env comment = %q", env["AUTH_REALM"].Comment)
	}

	js := keyed(t, "cfg.json", `{"server":{"timeout":30},"tags":["a","b"]}`)
	if js["server.timeout"].Value != "30" {
		t.Errorf("json = %+v", js)
	}
	if js["tags[0]"].Value != "a" {
		t.Errorf("json array = %+v", js)
	}
}

func TestUnparseableFileIsNotAnError(t *testing.T) {
	// A config file this pass cannot read is not a reason to fail a session.
	if got := Parse("cfg.json", []byte("{not json at all")); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

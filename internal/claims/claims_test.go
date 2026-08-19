package claims

import (
	"strings"
	"testing"
)

const yaml = `- id: c-001
  claim: "AuthCache.Get is idempotent for a fixed key within the TTL window"
  symbol: "internal/auth/cache.go::AuthCache.Get"
  fingerprint: "sha256:abc"
  executable: true
  test: |
    func TestC001(t *testing.T) {
        c := newTestCache(t)
        a, _ := c.Get("user:42")
        b, _ := c.Get("user:42")
        if a != b { t.Fatalf("not idempotent: %v vs %v", a, b) }
    }
- id: c-002
  claim: "Token refresh never blocks the request path"
  symbol: "internal/auth/cache.go::AuthCache.Refresh"
  executable: false        # surfaced as a trust-me assertion
`

func TestParseSpecExample(t *testing.T) {
	cs, err := Parse(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("got %d claims", len(cs))
	}
	if cs[0].ID != "c-001" || !cs[0].Executable {
		t.Errorf("c-001 = %+v", cs[0])
	}
	if cs[0].Symbol != "internal/auth/cache.go::AuthCache.Get" {
		t.Errorf("symbol = %q — the id contains :: and must survive parsing", cs[0].Symbol)
	}
	if !strings.Contains(cs[0].Test, "not idempotent") || !strings.HasPrefix(cs[0].Test, "func TestC001") {
		t.Errorf("block scalar mis-parsed:\n%s", cs[0].Test)
	}
	if cs[1].Executable {
		t.Error("c-002 is tagged executable: false and must stay that way")
	}
	if cs[1].Claim != "Token refresh never blocks the request path" {
		t.Errorf("claim = %q", cs[1].Claim)
	}
}

func TestRoundTrip(t *testing.T) {
	in, err := Parse(yaml)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(Render(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("round trip lost claims: %d -> %d", len(in), len(out))
	}
	for i := range in {
		if in[i].ID != out[i].ID || in[i].Claim != out[i].Claim || in[i].Symbol != out[i].Symbol ||
			in[i].Executable != out[i].Executable || strings.TrimSpace(in[i].Test) != strings.TrimSpace(out[i].Test) {
			t.Errorf("claim %d changed:\n%+v\n%+v", i, in[i], out[i])
		}
	}
}

func TestTestFuncName(t *testing.T) {
	if got := testFuncName("func TestC001(t *testing.T) {}"); got != "TestC001" {
		t.Errorf("got %q", got)
	}
	if got := testFuncName("not a test"); got != "" {
		t.Errorf("got %q", got)
	}
}

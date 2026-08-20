package stale

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/config"
	"github.com/k3-mt/plum/internal/lang"
	"github.com/k3-mt/plum/internal/lang/gopkg"
)

const original = `package auth

// Get returns the token.
func Get(key string) string {
	return key + "!"
}
`

func setup(t *testing.T, source string) (*config.Config, *lang.Registry, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cache.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default(root)
	reg := lang.NewRegistry(gopkg.New())

	a := gopkg.New()
	syms, err := a.ParseSymbols("cache.go", []byte(original))
	if err != nil {
		t.Fatal(err)
	}
	claimsPath := filepath.Join(root, "claims.yaml")
	if err := claims.Save(claimsPath, []claims.Claim{{
		ID: "c-001", Claim: "Get appends a bang", Symbol: syms[0].ID,
		Executable: true, Fingerprint: syms[0].Fingerprint,
	}}); err != nil {
		t.Fatal(err)
	}
	return cfg, reg, claimsPath
}

func TestFingerprintDrivesStaleness(t *testing.T) {
	// Unchanged code: the claim still addresses what it was written against.
	cfg, reg, path := setup(t, original)
	if got, err := Check(cfg, reg, path); err != nil || len(got) != 0 {
		t.Fatalf("unchanged: %v %v", got, err)
	}

	// Reformatted and re-commented: still not stale (P5).
	cfg, reg, path = setup(t, "package auth\n\n// Get returns the token, now with a longer comment.\nfunc Get(key string) string {\n\n\treturn key  +  \"!\"\n}\n")
	if got, err := Check(cfg, reg, path); err != nil || len(got) != 0 {
		t.Fatalf("reformatted should not be stale: %v %v", got, err)
	}

	// Logic changed: stale.
	cfg, reg, path = setup(t, "package auth\n\nfunc Get(key string) string {\n\treturn key + \"?\"\n}\n")
	got, err := Check(cfg, reg, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-001" {
		t.Fatalf("logic change should be stale, got %v", got)
	}

	// Symbol deleted outright.
	cfg, reg, path = setup(t, "package auth\n")
	got, err = Check(cfg, reg, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason == "" {
		t.Fatalf("deleted symbol should be stale, got %v", got)
	}
}

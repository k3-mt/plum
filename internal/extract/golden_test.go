package extract_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kelalaike/plum/internal/bundle"
	"github.com/kelalaike/plum/internal/config"
	"github.com/kelalaike/plum/internal/extract"
	"github.com/kelalaike/plum/internal/lang"
	"github.com/kelalaike/plum/internal/lang/gopkg"
	"github.com/kelalaike/plum/internal/vcs"
)

var update = flag.Bool("update", false, "regenerate the golden bundles in testdata/fixtures")

// TestGoldenBundles builds a real git repository from a fixture's before/after
// trees and asserts the extracted bundle byte-for-byte. Symbol mapping is the
// highest-value transform in the tool, so it is the one pinned hardest.
func TestGoldenBundles(t *testing.T) {
	fixtures, err := os.ReadDir("../../testdata/fixtures")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if !f.IsDir() {
			continue
		}
		t.Run(f.Name(), func(t *testing.T) {
			dir := filepath.Join("../../testdata/fixtures", f.Name())
			got := extractFixture(t, dir)
			goldenPath := filepath.Join(dir, "golden.json")

			data, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			if *update {
				if err := os.WriteFile(goldenPath, data, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Log("wrote", goldenPath)
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("%v (run: go test ./internal/extract -update)", err)
			}
			if string(data) != string(want) {
				t.Errorf("bundle differs from golden.\n--- got ---\n%s", data)
			}
		})
	}
}

// extractFixture stages before/ as the first commit and after/ as the session,
// then normalises everything machine-specific out of the bundle.
func extractFixture(t *testing.T, fixture string) *bundle.Bundle {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	copyDir(t, filepath.Join(fixture, "before"), root)
	run("git", "init", "-q")
	run("git", "config", "user.email", "fixture@plum.test")
	run("git", "config", "user.name", "fixture")
	run("git", "add", "-A")
	run("git", "commit", "-qm", "before")

	repo := vcs.New(root)
	ctx := context.Background()
	start, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	copyDir(t, filepath.Join(fixture, "after"), root)
	run("git", "add", "-A")
	run("git", "commit", "-qm", "after")
	end, err := repo.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default(root)
	reg := lang.NewRegistry(gopkg.New())
	sess := bundle.Session{
		ID: "fixture", StartSHA: start, EndSHA: end,
		StartedAt: time.Unix(0, 0).UTC(), EndedAt: time.Unix(0, 0).UTC(),
		Command: "fixture", Agent: "fixture",
	}
	b, err := extract.New(repo, cfg, reg).Extract(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	// SHAs and the temp path are machine-specific; everything else is evidence.
	b.Session.StartSHA, b.Session.EndSHA, b.Session.Repo = "<start>", "<end>", "<root>"
	return b
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

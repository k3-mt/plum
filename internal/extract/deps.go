package extract

import (
	"bufio"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/kelalaike/plum/internal/bundle"
)

// lockfiles are read at both ends of the range and diffed by name. A new
// dependency is a gate-worthy event on its own: it is supply chain the reviewer
// did not choose.
var lockfiles = []struct {
	path      string
	ecosystem string
	parse     func(string) map[string]string
}{
	{"go.mod", "go", parseGoMod},
	{"go.sum", "go", nil}, // checksums only — names come from go.mod
	{"package.json", "npm", parsePackageJSON},
	{"package-lock.json", "npm", nil},
	{"requirements.txt", "pypi", parseRequirements},
	{"pyproject.toml", "pypi", nil},
	{"Cargo.toml", "cargo", nil},
}

func (e *Extractor) deps(ctx context.Context, b *bundle.Bundle, sess bundle.Session) {
	for _, lf := range lockfiles {
		if lf.parse == nil {
			continue
		}
		beforeSrc, _ := e.Repo.Show(ctx, sess.StartSHA, lf.path)
		afterSrc, _ := e.Repo.Show(ctx, sess.EndSHA, lf.path)
		if beforeSrc == "" && afterSrc == "" {
			continue
		}
		before, after := lf.parse(beforeSrc), lf.parse(afterSrc)
		for name, ver := range after {
			prev, ok := before[name]
			switch {
			case !ok:
				b.Deps.Added = append(b.Deps.Added, bundle.Dep{Ecosystem: lf.ecosystem, Name: name, Version: ver})
			case prev != ver:
				b.Deps.Upgraded = append(b.Deps.Upgraded, bundle.DepMod{
					Dep:    bundle.Dep{Ecosystem: lf.ecosystem, Name: name, Version: ver},
					Before: prev, After: ver,
				})
			}
		}
		for name, ver := range before {
			if _, ok := after[name]; !ok {
				b.Deps.Removed = append(b.Deps.Removed, bundle.Dep{Ecosystem: lf.ecosystem, Name: name, Version: ver})
			}
		}
	}
	sort.Slice(b.Deps.Added, func(i, j int) bool { return b.Deps.Added[i].Name < b.Deps.Added[j].Name })
	sort.Slice(b.Deps.Removed, func(i, j int) bool { return b.Deps.Removed[i].Name < b.Deps.Removed[j].Name })
	sort.Slice(b.Deps.Upgraded, func(i, j int) bool { return b.Deps.Upgraded[i].Name < b.Deps.Upgraded[j].Name })
}

func parseGoMod(src string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(src))
	inBlock := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !inBlock:
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			out[f[0]] = f[1]
		}
	}
	return out
}

func parsePackageJSON(src string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(src) == "" {
		return out
	}
	var pkg struct {
		Deps    map[string]string `json:"dependencies"`
		DevDeps map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal([]byte(src), &pkg); err != nil {
		return out
	}
	for k, v := range pkg.Deps {
		out[k] = v
	}
	for k, v := range pkg.DevDeps {
		out[k+" (dev)"] = v
	}
	return out
}

func parseRequirements(src string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "-") {
			continue
		}
		for _, sep := range []string{"==", ">=", "~=", "<="} {
			if i := strings.Index(l, sep); i > 0 {
				out[strings.TrimSpace(l[:i])] = strings.TrimSpace(l[i+2:])
				goto next
			}
		}
		out[l] = "*"
	next:
	}
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func itoa(n int) string { return strconv.Itoa(n) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

package gopkg

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/k3-mt/plum/internal/bundle"
)

// PublicSurface reports the things that break other people when they change:
// exported declarations, HTTP routes, environment variables and CLI flags.
func (a *Adapter) PublicSurface(path string, src []byte) ([]bundle.SurfaceItem, error) {
	p, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	rel := filepath.ToSlash(path)
	var out []bundle.SurfaceItem

	add := func(kind, name, sig string, id bundle.SymbolID) {
		out = append(out, bundle.SurfaceItem{Kind: kind, Name: name, File: rel, Signature: sig, Symbol: id})
	}

	for _, d := range p.file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if !ast.IsExported(decl.Name.Name) {
				continue
			}
			qual := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				recv := receiverName(p, decl.Recv.List[0].Type)
				if !ast.IsExported(recv) {
					continue // a method on an unexported type is not reachable surface
				}
				qual = recv + "." + decl.Name.Name
			}
			add("export", p.file.Name.Name+"."+qual, signature(p, decl), bundle.MakeID(rel, qual))
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(s.Name.Name) {
						add("export", p.file.Name.Name+"."+s.Name.Name, "type "+s.Name.Name+" "+p.text(s.Type), bundle.MakeID(rel, s.Name.Name))
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if ast.IsExported(n.Name) {
							kind := "var"
							if decl.Tok == token.CONST {
								kind = "const"
							}
							add("export", p.file.Name.Name+"."+n.Name, kind+" "+p.text(s), bundle.MakeID(rel, n.Name))
						}
					}
				}
			}
		}
	}

	// Routes, env vars and flags are call-shaped, so they are found by walking
	// call expressions rather than declarations.
	ast.Inspect(p.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call.Fun)
		short := lastSegment(name)
		switch {
		case short == "Getenv" || short == "LookupEnv":
			if v := literalArg(call, 0); v != "" {
				add("env_var", v, name, "")
			}
		case strings.HasPrefix(short, "Handle") || short == "Route":
			if v := literalArg(call, 0); v != "" {
				add("route", v, name, "")
			}
		case isRouterVerb(name, short):
			if v := literalArg(call, 0); strings.HasPrefix(v, "/") {
				add("route", strings.ToUpper(short)+" "+v, name, "")
			}
		case isFlagCall(name):
			if v := literalArg(call, 0); v != "" {
				add("cli_flag", "--"+v, name, "")
			}
		}
		return true
	})
	return out, nil
}

func isRouterVerb(full, short string) bool {
	switch short {
	case "Get", "Post", "Put", "Patch", "Delete", "Head", "Options":
	default:
		return false
	}
	// Only treat it as a route when it is called on something router-ish; a bare
	// cache.Get must not register as an HTTP route.
	lower := strings.ToLower(full)
	for _, hint := range []string{"router", "mux", "app", "engine", "group", "r.", "e."} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func isFlagCall(full string) bool {
	if !strings.Contains(full, "flag.") && !strings.Contains(full, "fs.") && !strings.Contains(full, "Flags().") {
		return false
	}
	short := lastSegment(full)
	switch short {
	case "String", "StringVar", "Bool", "BoolVar", "Int", "IntVar", "Duration", "DurationVar", "Float64", "Float64Var":
		return true
	}
	return false
}

func literalArg(call *ast.CallExpr, i int) string {
	if len(call.Args) <= i {
		return ""
	}
	lit, ok := call.Args[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return v
}

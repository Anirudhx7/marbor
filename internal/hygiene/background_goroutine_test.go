package hygiene

// background_goroutine_test.go: a panic in any goroutine kills the whole
// process, and marbor is one process for the entire fleet. Every goroutine the
// router starts must recover, either inline, through safeRun, or by calling a
// same-package function that does. A nil-probe panic in a poll goroutine once
// crash-looped the process until the database was deleted.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// unprotectedGoroutines lists known gaps: functions that still start a goroutine
// without recover. Each entry is a bug awaiting a fix, not an exemption. The
// test fails when an entry becomes protected, so fixing the function forces
// the entry's removal.
var unprotectedGoroutines = map[string]bool{
	"RecordPrefixLocality": true, // store-write goroutine; wrap it in safeRun, then delete this line
}

func callsRecover(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && (id.Name == "recover" || id.Name == "safeRun") {
				found = true
			}
		}
		return !found
	})
	return found
}

func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func TestRouterGoroutinesRecoverFromPanics(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "router", "*.go"))
	fset := token.NewFileSet()
	var parsed []*ast.File
	safeDecls := map[string]bool{"safeRun": true} // function names whose body recovers
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		parsed = append(parsed, f)
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil && callsRecover(fn.Body) {
				safeDecls[fn.Name.Name] = true
			}
		}
	}
	if len(parsed) == 0 {
		t.Fatal("no router source files found")
	}
	var offenders []string
	gapSeen := map[string]bool{}
	for _, f := range parsed {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			known := unprotectedGoroutines[fn.Name.Name]
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				g, ok := n.(*ast.GoStmt)
				if !ok || safeDecls[calleeName(g.Call)] {
					return true
				}
				protected := false
				if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
					protected = callsRecover(lit.Body)
					ast.Inspect(lit.Body, func(m ast.Node) bool {
						if c, ok := m.(*ast.CallExpr); ok && safeDecls[calleeName(c)] {
							protected = true
						}
						return !protected
					})
				}
				if !protected && known {
					gapSeen[fn.Name.Name] = true
				} else if !protected {
					offenders = append(offenders, fset.Position(g.Pos()).String()+" in "+fn.Name.Name)
				}
				return true
			})
		}
	}
	for name := range unprotectedGoroutines {
		if !gapSeen[name] {
			t.Errorf("%s is listed as an unprotected goroutine but no longer is (or no longer exists): delete its entry from unprotectedGoroutines", name)
		}
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s: goroutine neither recovers nor calls a function that does. Wrap it in safeRun or recover inline; an unrecovered panic takes down every node.", o)
	}
}

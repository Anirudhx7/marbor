package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// lockedVar reports the variable whose lock stmt takes (n.RLock(), n.Lock(),
// n.mu.RLock() or n.mu.Lock()) and whether stmt is such a call at all.
func lockedVar(stmt ast.Stmt, names ...string) (string, bool) {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return "", false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	match := false
	for _, nm := range names {
		if sel.Sel.Name == nm {
			match = true
		}
	}
	if !match {
		return "", false
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name, true
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && x.Sel.Name == "mu" {
			return id.Name, true
		}
	}
	return "", false
}

// heldRegions walks every block under root. Where a statement takes the lock
// on a variable, it hands onFlag the statements that run while that lock is
// held: up to the matching unlock in the same block, or to the end of the
// block when the unlock is deferred or absent.
func heldRegions(root ast.Node, onRegion func(v string, held []ast.Stmt)) {
	ast.Inspect(root, func(x ast.Node) bool {
		var list []ast.Stmt
		switch b := x.(type) {
		case *ast.BlockStmt:
			list = b.List
		case *ast.CaseClause:
			list = b.Body
		case *ast.CommClause:
			list = b.Body
		default:
			return true
		}
		for i, st := range list {
			v, ok := lockedVar(st, "RLock", "Lock")
			if !ok {
				continue
			}
			end := len(list)
			for j := i + 1; j < len(list); j++ {
				if u, ok := lockedVar(list[j], "RUnlock", "Unlock"); ok && u == v {
					end = j
					break
				}
			}
			onRegion(v, list[i+1:end])
		}
		return true
	})
}

// selfLockingCallsUnderLock returns "var.Method" for each self-locking method
// called on the locked variable inside the held statements.
func selfLockingCallsUnderLock(held []ast.Stmt, v string, selfLocking map[string]bool) []string {
	var hits []string
	for _, st := range held {
		ast.Inspect(st, func(x ast.Node) bool {
			sel, ok := x.(*ast.SelectorExpr)
			if !ok || !selfLocking[sel.Sel.Name] {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == v {
				hits = append(hits, v+"."+sel.Sel.Name)
			}
			return true
		})
	}
	return hits
}

// offendersIn lists "func: var.Method" for every self-locking call made on a
// variable between its lock and unlock in the same block.
func offendersIn(files []*ast.File, selfLocking map[string]bool) []string {
	var out []string
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			heldRegions(fd.Body, func(v string, held []ast.Stmt) {
				for _, h := range selfLockingCallsUnderLock(held, v, selfLocking) {
					out = append(out, fd.Name.Name+": "+h)
				}
			})
		}
	}
	sort.Strings(out)
	return out
}

// Wider sibling of TestNoSelfLockingAccessorsUnderNodeLock. That test scans
// five named functions; this one walks every function in the router and admin
// packages and fails when a node method that takes the node lock itself is
// called on a variable between that variable's RLock/Lock and its unlock in the
// same block (a deferred unlock holds to the end of the block). Limits: a call
// reached through another helper, or on a node held under a different name than
// the locked variable, is not seen.
func TestNoSelfLockingAccessorsBetweenLockAndUnlock(t *testing.T) {
	routerFiles := parseGoDir(t, ".")
	adminFiles := parseGoDir(t, filepath.Join("..", "admin"))
	selfLocking := lockingNodeMethods(t, routerFiles)
	if !selfLocking["GetRuntime"] || !selfLocking["EffectiveRequiredGPUs"] {
		t.Fatalf("derived self-locking set looks wrong: %v", selfLocking)
	}
	all := append(append([]*ast.File{}, routerFiles...), adminFiles...)
	for _, o := range offendersIn(all, selfLocking) {
		t.Errorf("self-locking call while the node lock is held: %s", o)
	}
}

func parseSnippet(t *testing.T, body string) []*ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "snippet.go", "package p\n"+body, 0)
	if err != nil {
		t.Fatal(err)
	}
	return []*ast.File{f}
}

// The walk must flag a seeded bad pattern and stay quiet on safe ones.
func TestNodeLockRegionWalk_SeededOffenders(t *testing.T) {
	selfLocking := map[string]bool{"GetRuntime": true}
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"read lock then accessor", "func f(n *N) { n.RLock(); _ = n.GetRuntime(); n.RUnlock() }", []string{"f: n.GetRuntime"}},
		{"mu field lock", "func f(n *N) { n.mu.RLock(); _ = n.GetRuntime(); n.mu.RUnlock() }", []string{"f: n.GetRuntime"}},
		{"write lock", "func f(n *N) { n.Lock(); _ = n.GetRuntime(); n.Unlock() }", []string{"f: n.GetRuntime"}},
		{"deferred unlock holds to block end", "func f(n *N) { n.RLock(); defer n.RUnlock(); _ = n.GetRuntime() }", []string{"f: n.GetRuntime"}},
		{"nested block and method value", "func f(n *N) { n.RLock(); if true { g := n.GetRuntime; _ = g }; n.RUnlock() }", []string{"f: n.GetRuntime"}},
		{"in loop body", "func f(ns []*N) { for _, x := range ns { x.RLock(); _ = x.GetRuntime(); x.RUnlock() } }", []string{"f: x.GetRuntime"}},
		{"in case clause", "func f(n *N) { switch { case true: n.RLock(); _ = n.GetRuntime(); n.RUnlock() } }", []string{"f: n.GetRuntime"}},
		{"after unlock is fine", "func f(n *N) { n.RLock(); n.RUnlock(); _ = n.GetRuntime() }", nil},
		{"before lock is fine", "func f(n *N) { _ = n.GetRuntime(); n.RLock(); n.RUnlock() }", nil},
		{"other variable is fine", "func f(n, m *N) { n.RLock(); _ = m.GetRuntime(); n.RUnlock() }", nil},
		{"non-self-locking method is fine", "func f(n *N) { n.RLock(); _ = n.Name; n.RUnlock() }", nil},
	}
	for _, c := range cases {
		got := offendersIn(parseSnippet(t, c.body), selfLocking)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The wide guard must see a real number of lock regions, so an AST change
// that silently empties the walk cannot leave it passing on nothing.
func TestNodeLockRegionWalk_SeesRealRegions(t *testing.T) {
	files := append(parseGoDir(t, "."), parseGoDir(t, filepath.Join("..", "admin"))...)
	regions := 0
	for _, f := range files {
		heldRegions(f, func(string, []ast.Stmt) { regions++ })
	}
	if regions < 40 {
		t.Fatalf("walk found only %d lock regions in router and admin; expected many more", regions)
	}
}

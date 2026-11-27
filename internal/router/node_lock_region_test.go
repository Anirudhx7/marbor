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
// on a variable, it hands onRegion the statements that run while that lock is
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

// unlocksVar reports whether stmt is a plain (not deferred) unlock of v.
func unlocksVar(stmt ast.Stmt, v string) bool {
	u, ok := lockedVar(stmt, "RUnlock", "Unlock")
	return ok && u == v
}

// scanHeld collects "v.Method" for each self-locking method called on v in the
// statements. Function literals are skipped (they may run after the unlock). A
// nested block that unlocks v stops being scanned at that unlock, so an early
// "unlock, then return" branch is not treated as still holding the lock.
func scanHeld(stmts []ast.Stmt, v string, selfLocking map[string]bool, hits *[]string) {
	for _, st := range stmts {
		ast.Inspect(st, func(x ast.Node) bool {
			switch n := x.(type) {
			case *ast.FuncLit:
				return false
			case *ast.BlockStmt:
				for i, inner := range n.List {
					if unlocksVar(inner, v) {
						scanHeld(n.List[:i], v, selfLocking, hits)
						return false
					}
				}
			case *ast.SelectorExpr:
				if !selfLocking[n.Sel.Name] {
					return true
				}
				if id, ok := n.X.(*ast.Ident); ok && id.Name == v {
					*hits = append(*hits, v+"."+n.Sel.Name)
				}
			}
			return true
		})
	}
}

// selfLockingCallsUnderLock returns "var.Method" for each self-locking method
// called on the locked variable inside the held statements.
func selfLockingCallsUnderLock(held []ast.Stmt, v string, selfLocking map[string]bool) []string {
	var hits []string
	scanHeld(held, v, selfLocking, &hits)
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
// same block (a deferred unlock holds to the end of the block; a nested block
// that unlocks is cut at the unlock; function literals are skipped). The
// self-locking set comes from the router package only. Limits: a call
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
		{"in select comm clause", "func f(n *N, c chan int) { select { case <-c: n.RLock(); _ = n.GetRuntime(); n.RUnlock() } }", []string{"f: n.GetRuntime"}},
		{"two offenders in one region", "func f(n *N) { n.RLock(); _ = n.GetRuntime(); _ = n.GetRuntime(); n.RUnlock() }", []string{"f: n.GetRuntime", "f: n.GetRuntime"}},
		{"early unlock and return in nested block is fine", "func f(n *N, c bool) { n.RLock(); if c { n.RUnlock(); return n.GetRuntime() }; n.RUnlock() }", nil},
		{"call after nested early unlock is still held", "func f(n *N, c bool) { n.RLock(); if c { n.RUnlock(); return }; _ = n.GetRuntime(); n.RUnlock() }", []string{"f: n.GetRuntime"}},
		{"closure under lock is skipped", "func f(n *N) { n.RLock(); go func() { _ = n.GetRuntime() }(); n.RUnlock() }", nil},
		{"unlock of another variable keeps the hold", "func f(n, m *N) { n.RLock(); m.RUnlock(); _ = n.GetRuntime() }", []string{"f: n.GetRuntime"}},
		{"relock after unlock then call", "func f(n *N) { n.RLock(); n.RUnlock(); n.RLock(); _ = n.GetRuntime(); n.RUnlock() }", []string{"f: n.GetRuntime"}},
		{"after unlock is fine", "func f(n *N) { n.RLock(); n.RUnlock(); _ = n.GetRuntime() }", nil},
		{"before lock is fine", "func f(n *N) { _ = n.GetRuntime(); n.RLock(); n.RUnlock() }", nil},
		{"other variable is fine", "func f(n, m *N) { n.RLock(); _ = m.GetRuntime(); n.RUnlock() }", nil},
		{"non-self-locking method is fine", "func f(n *N) { n.RLock(); _ = n.Name; n.RUnlock() }", nil},
	}
	for _, c := range cases {
		c := c
		got := offendersIn(parseSnippet(t, c.body), selfLocking)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The wide guard must see a real number of node lock regions, so an AST change
// that silently empties the walk cannot leave it passing on nothing. Only
// regions on a variable that the enclosing function treats as a node count.
func TestNodeLockRegionWalk_SeesRealRegions(t *testing.T) {
	files := append(parseGoDir(t, "."), parseGoDir(t, filepath.Join("..", "admin"))...)
	nodeRegions := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			idents, _ := nodeIdentsOf(fd)
			heldRegions(fd.Body, func(v string, _ []ast.Stmt) {
				if idents[v] {
					nodeRegions++
				}
			})
		}
	}
	if nodeRegions < 15 {
		t.Fatalf("walk found only %d node lock regions in router and admin; expected at least 15", nodeRegions)
	}
}

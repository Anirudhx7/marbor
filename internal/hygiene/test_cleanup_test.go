package hygiene

// test_cleanup_test.go guards a flake that has now hit three separate proxy
// test rigs. admin.NewServer given a store starts a goroutine that writes
// request records into that store from a channel. A test that builds one over a
// store living in t.TempDir() and never calls Shutdown leaves that goroutine
// running after the test returns, so it can write to the database while the
// temp directory is being removed: "TempDir RemoveAll cleanup: directory not
// empty" plus "store: AppendRequest trim: sql: database is closed" in the log,
// and a red main CI on a timing-dependent run.
//
// The fix is always the same - stop the server before the store closes, e.g.
// t.Cleanup(a.Shutdown) registered after the store's own Close cleanup - and
// forgetting it is easy, so this checks for it instead of relying on memory.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// helpersThatHandBackTheServer are test helpers that build an admin server over
// a store the caller owns and return the server, so the calling test shuts it
// down. Keep this list short; a new entry needs a reason like these.
var helpersThatHandBackTheServer = map[string]bool{
	"newAliasHandler": true, // returns the admin server; its callers defer Shutdown
}

func TestProxyTestsShutDownAdminServersThatOwnAStore(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "proxy", "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no proxy test files found (err=%v)", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || helpersThatHandBackTheServer[fn.Name.Name] {
				continue
			}
			var storeBacked token.Pos
			shutsDown := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "admin" && sel.Sel.Name == "NewServer" && len(x.Args) >= 4 && storeBacked == token.NoPos {
							storeBacked = x.Pos()
						}
					}
				case *ast.SelectorExpr:
					// Matches a.Shutdown() and the method value t.Cleanup(a.Shutdown).
					if x.Sel.Name == "Shutdown" {
						shutsDown = true
					}
				}
				return true
			})
			if storeBacked != token.NoPos && !shutsDown {
				offenders = append(offenders, fset.Position(storeBacked).String()+" in "+fn.Name.Name)
			}
		}
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s: admin.NewServer is given a store but the function never calls Shutdown. Its async request logger can still be writing after the store closes and the temp dir is removed. Register t.Cleanup(a.Shutdown) after the store's Close cleanup (see newCapRig).", o)
	}
}

package hygiene

// log_quoting_test.go: a node name or URL is operator-supplied text. Written
// with a bare %s it can carry a newline, a carriage return or an escape byte
// straight into the log and forge or split lines. Log calls in the router and
// admin packages must format them with %q.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bareNameOrURLVerb matches "node ... %s" or "url ... %s" in a log format
// string with no other verb in between, so "node %q model %s" is not flagged
// but "node %s", "node override for %s" and "url=%s" are.
var bareNameOrURLVerb = regexp.MustCompile(`(?i)\b(?:nodes?|url)\b[^%]*%s`)

// formatVerb matches one printf verb; %% never consumes an argument.
var formatVerb = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z%]`)

// nodeOrURLArgNames are identifiers that hold a node name or URL in the
// router and admin packages. The format text alone cannot always tell
// ("warmstate: reconcile %s"), so a %s paired with one of these is flagged too.
var nodeOrURLArgNames = map[string]bool{"node": true, "nodeName": true, "url": true, "nodeURL": true}

var loggerCalls = map[string]bool{
	"Printf": true, "Print": true, "Println": true,
	"Fatalf": true, "Fatal": true, "Panicf": true, "Panic": true,
}

// stringLiteralsOf returns every string literal inside expr, so a format
// string built by concatenation is checked as a whole.
func stringLiteralsOf(expr ast.Expr) string {
	var b strings.Builder
	ast.Inspect(expr, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				b.WriteString(s)
			}
		}
		return true
	})
	return b.String()
}

// bareVerbOnNodeOrURLArg reports whether a %s verb in format is paired with
// an argument that is a node name or URL.
//
// In the router package a plain `name` is always a node name; in admin it can
// be a backup or model name, so there it is not treated as one.
func bareVerbOnNodeOrURLArg(format string, args []ast.Expr, nameIsNode bool) bool {
	for i, verb := range formatVerb.FindAllString(format, -1) {
		if verb == "%%" || verb != "%s" || i >= len(args) {
			continue
		}
		switch a := args[i].(type) {
		case *ast.Ident:
			if nodeOrURLArgNames[a.Name] || (nameIsNode && a.Name == "name") {
				return true
			}
		case *ast.SelectorExpr:
			if a.Sel.Name == "URL" {
				return true
			}
		}
	}
	return false
}

func isLogPackageCall(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || !loggerCalls[sel.Sel.Name] {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "log"
}

func TestLogCallsQuoteNodeNamesAndURLs(t *testing.T) {
	var files []string
	for _, pkg := range []string{"router", "admin"} {
		matches, _ := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		files = append(files, matches...)
	}
	fset := token.NewFileSet()
	var offenders []string
	parsedAny := false
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		parsedAny = true
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || len(c.Args) == 0 || !isLogPackageCall(c) {
				return true
			}
			if format := stringLiteralsOf(c.Args[0]); bareNameOrURLVerb.MatchString(format) || bareVerbOnNodeOrURLArg(format, c.Args[1:], strings.Contains(filepath.ToSlash(p), "/router/")) {
				offenders = append(offenders, fset.Position(c.Pos()).String()+": "+strconv.Quote(format))
			}
			return true
		})
	}
	if !parsedAny {
		t.Fatal("no router or admin source files found")
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s\n\tformats a node name or URL with a bare %%s; use %%q so odd characters cannot forge or split log lines", o)
	}
}

func TestBareNameOrURLVerbPattern(t *testing.T) {
	flagged := []string{
		"node %s: bad", "headroom: node %s runs %q", "failed to persist node override for %s: %v",
		"request to node %s failed", "url=%s", "refusing %s URL %s",
	}
	allowed := []string{
		"node %q: bad", "node %q model %s", "node %q at %s", "schedule %q fired but node %q was not found: action=%s",
		"unloaded model %q from node %q (%s)",
	}
	for _, s := range flagged {
		if !bareNameOrURLVerb.MatchString(s) {
			t.Errorf("%q should be flagged", s)
		}
	}
	for _, s := range allowed {
		if bareNameOrURLVerb.MatchString(s) {
			t.Errorf("%q should not be flagged", s)
		}
	}
}

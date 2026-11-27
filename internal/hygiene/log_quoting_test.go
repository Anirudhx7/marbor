package hygiene

// log_quoting_test.go: a node name or URL is operator-supplied text. Written
// with a bare %s or %v it can carry a newline, a carriage return or an escape
// byte straight into the log and forge or split lines. Log calls in the router
// and admin packages must format them with %q.
//
// The check is deliberately limited: it reads direct calls to the standard log
// package and the router's logAgentPoll helper in the router and admin
// packages. It does not follow a fmt.Sprintf whose result is logged later, a
// format held in a variable, or other packages.

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

// formatVerb matches one printf verb, capturing a `*` width or precision
// (each consumes an argument of its own). %% never consumes an argument.
var formatVerb = regexp.MustCompile(`%[-+# 0]*(\*|\d+)?(?:\.(\*|\d+)?)?[a-zA-Z%]`)

// nodeOrURLArgNames are identifiers that hold a node name, URL or host in the
// router and admin packages. The format text alone cannot always tell
// ("warmstate: reconcile %s"), so a bare verb paired with one of these is
// flagged too.
var nodeOrURLArgNames = map[string]bool{"node": true, "nodeName": true, "url": true, "nodeURL": true, "host": true}

// formatCalls maps a call to the position of its format string.
var formatCalls = map[string]int{
	"log.Printf": 0, "log.Fatalf": 0, "log.Panicf": 0,
	"logAgentPoll": 2,
}

// valueCalls take plain values rather than a format string.
var valueCalls = map[string]bool{
	"log.Print": true, "log.Println": true, "log.Fatal": true, "log.Fatalln": true,
	"log.Panic": true, "log.Panicln": true,
}

// callName returns "log.Func" for a call on the log package and the bare
// method name for any other selector call.
func callName(c *ast.CallExpr) string {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "log" {
		return "log." + sel.Sel.Name
	}
	return sel.Sel.Name
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

// isNodeOrURLExpr reports whether expr mentions a node name, URL or host
// anywhere, so a call such as strings.TrimRight(cfg.URL, "/") is seen
// through. A selector named Node, URL or Host always counts. Name counts only
// where nameIsNode is set: in the router package a plain name is a node name;
// in admin it can be a backup or model name.
func isNodeOrURLExpr(expr ast.Expr, nameIsNode bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch a := n.(type) {
		case *ast.Ident:
			found = found || nodeOrURLArgNames[a.Name] || (nameIsNode && a.Name == "name")
		case *ast.SelectorExpr:
			if inner, ok := a.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "URL" {
				return false // r.URL.Path is a request path, not a node URL
			}
			found = found || a.Sel.Name == "URL" || a.Sel.Name == "Node" || a.Sel.Name == "Host" ||
				(nameIsNode && a.Sel.Name == "Name")
		}
		return !found
	})
	return found
}

// bareVerbOnNodeOrURLArg reports whether a %s or %v verb in format is paired
// with an argument that is a node name or URL. Explicit argument indexes
// ("%[1]s") cannot be paired by position, so such a call is skipped.
func bareVerbOnNodeOrURLArg(format string, args []ast.Expr, nameIsNode bool) bool {
	if strings.Contains(format, "%[") {
		return false
	}
	pos := 0
	for _, m := range formatVerb.FindAllStringSubmatch(format, -1) {
		verb := m[0]
		if verb == "%%" {
			continue
		}
		pos += strings.Count(verb, "*")
		last := verb[len(verb)-1]
		if (last == 's' || last == 'v') && pos < len(args) && isNodeOrURLExpr(args[pos], nameIsNode) {
			return true
		}
		pos++
	}
	return false
}

// offenderFor describes call when it logs a node name or URL bare, else "".
func offenderFor(c *ast.CallExpr, nameIsNode bool) string {
	name := callName(c)
	if idx, ok := formatCalls[name]; ok {
		if len(c.Args) <= idx {
			return ""
		}
		format := stringLiteralsOf(c.Args[idx])
		if bareNameOrURLVerb.MatchString(format) || bareVerbOnNodeOrURLArg(format, c.Args[idx+1:], nameIsNode) {
			return strconv.Quote(format)
		}
		return ""
	}
	if valueCalls[name] {
		for _, a := range c.Args {
			if isNodeOrURLExpr(a, nameIsNode) {
				return "a node name or URL passed as a plain value to " + name
			}
		}
	}
	return ""
}

// bareLogCalls returns one description per call in f that logs a node name or
// URL without quoting it.
func bareLogCalls(fset *token.FileSet, f *ast.File, nameIsNode bool) []string {
	var offenders []string
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if what := offenderFor(c, nameIsNode); what != "" {
				offenders = append(offenders, fset.Position(c.Pos()).String()+": "+what)
			}
		}
		return true
	})
	return offenders
}

func TestLogCallsQuoteNodeNamesAndURLs(t *testing.T) {
	var files []string
	for _, pkg := range []string{"router", "admin"} {
		matches, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil {
			t.Fatalf("globbing %s: %v", pkg, err)
		}
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
		inRouter := strings.Contains(filepath.ToSlash(p), "/router/")
		offenders = append(offenders, bareLogCalls(fset, f, inRouter)...)
	}
	if !parsedAny {
		t.Fatal("no router or admin source files found")
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s: use %%q for node names and URLs so odd characters cannot forge or split log lines", o)
	}
}

func TestBareLogCallsFlagsSeededOffenders(t *testing.T) {
	const src = `package x
func f(s S, cfg C, n N, m M, r R, name string, err error) {
	log.Printf("persist drain for %s: %v", s.Node, err)
	log.Printf("webhook: post %s: %v", strings.TrimRight(cfg.URL, "/"), err)
	log.Printf("warmstate: record load %q on %s: %v", m.Name, n.Name, err)
	log.Printf("width %*d on %s", 4, 1, name)
	log.Printf("100%% done on %s", name)
	log.Printf("probe %v failed", name)
	log.Println("probing", s.Node)
	r.logAgentPoll(cfg.Host, "k", "reached %s", n.Name)
	log.Printf("node %q at %q", n.Name, cfg.URL)
	log.Printf("model %s on %q", m.Name, n.Name)
	log.Printf("indexed %[2]s %[1]s", name, "x")
	log.Println("done", err)
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "seed.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(bareLogCalls(fset, f, true), "\n")
	wantFlagged := []string{"persist drain", "webhook: post", "record load", "width", "100%% done", "probe %v", "plain value to log.Println", "reached %s"}
	for _, want := range wantFlagged {
		if !strings.Contains(got, want) {
			t.Errorf("seeded offender %q not flagged in:\n%s", want, got)
		}
	}
	for _, clean := range []string{"node %q at", "indexed"} {
		if strings.Contains(got, clean) {
			t.Errorf("clean call %q wrongly flagged in:\n%s", clean, got)
		}
	}
	admin := strings.Join(bareLogCalls(fset, f, false), "\n")
	if strings.Contains(admin, "record load") || strings.Contains(admin, "probe %v") {
		t.Errorf("in the admin package a plain Name must not count as a node name:\n%s", admin)
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

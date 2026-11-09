package hikyaku_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoImplicitOutboundTransport fails when non-test code in this module
// could send a request to a backend without the server's outbound transport.
// Every outbound request goes through that one transport (an embedder's
// TransportFactory, or core's default with the build's TLS floor and no
// HTTP_PROXY): live traffic, balancer probes, /api/tags and the startup
// probes alike. The forms below reach Go's default transport instead, which
// honors HTTP_PROXY, has no TLS floor and bypasses a TransportFactory:
//
//   - http.DefaultClient, http.DefaultTransport, http.ProxyFromEnvironment;
//   - http.Get, http.Head, http.Post, http.PostForm (the default client);
//   - an http.Client without a Transport (literal, new(http.Client), a
//     zero-value variable or a value-type struct field);
//   - an httputil.ReverseProxy literal without a Transport, and
//     httputil.NewSingleHostReverseProxy;
//   - a dot-import of net/http, which would hide every form above.
//
// A transport built anywhere else is a second outbound transport that a
// TransportFactory does not replace, so every http.Transport literal and
// new(http.Transport) is flagged too, except in the two functions that build
// core's default (defaultTransportBuilders).
func TestNoImplicitOutboundTransport(t *testing.T) {
	var findings []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		findings = append(findings, implicitTransportUses(fset, f, filepath.ToSlash(path))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

// TestImplicitTransportUses_DetectsEveryForm proves the scanner above flags
// each form it is meant to, under the package's own name and an import alias,
// and accepts the explicit-transport forms.
func TestImplicitTransportUses_DetectsEveryForm(t *testing.T) {
	const header = `package p

import (
	"net/http"
	nh "net/http"
	"net/http/httputil"
	"time"
)

var rt http.RoundTripper
var _ = time.Second

`
	bad := []struct {
		stmt string
		want int
	}{
		{"_ = http.DefaultClient", 1},
		{"_ = nh.DefaultTransport", 1},
		{"_ = http.Transport{Proxy: http.ProxyFromEnvironment}", 2},
		{`_, _ = http.Get("u")`, 1},
		{`_, _ = nh.Head("u")`, 1},
		{`_, _ = http.Post("u", "", nil)`, 1},
		{`_, _ = http.PostForm("u", nil)`, 1},
		{"_ = &http.Client{Timeout: 1}", 1},
		{"_ = new(http.Client)", 1},
		{"var c http.Client; _ = c", 1},
		{"type T struct{ c http.Client }; _ = T{}", 1},
		{"_ = &httputil.ReverseProxy{}", 1},
		{"_ = httputil.NewSingleHostReverseProxy(nil)", 1},
		{"_ = http.Transport{}", 1},
		{"_ = new(nh.Transport)", 1},
		// The startup probe's form before every backend request shared the
		// server's transport: its own transport, which a factory never sees.
		{"_ = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: nil}}", 1},
	}
	good := []string{
		"_ = &http.Client{Transport: rt}",
		"_ = &nh.Client{Transport: rt, Timeout: 1}",
		"_ = &httputil.ReverseProxy{Transport: rt}",
		"var c *http.Client; _ = c",
		"type T struct{ c *http.Client }; _ = T{}",
		"var tr *http.Transport; _ = tr",
	}
	scan := func(stmt string) []string {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", header+"func f() { "+stmt+" }\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return implicitTransportUses(fset, f, "p.go")
	}
	for _, tc := range bad {
		if got := scan(tc.stmt); len(got) != tc.want {
			t.Errorf("%s: scanner found %d use(s), want %d: %v", tc.stmt, len(got), tc.want, got)
		}
	}
	for _, stmt := range good {
		if got := scan(stmt); len(got) != 0 {
			t.Errorf("%s: scanner flagged an explicit-transport form: %v", stmt, got)
		}
	}
}

// A dot-import of net/http is flagged: a bare DefaultClient after it would
// escape every other check.
func TestImplicitTransportUses_FlagsDotImport(t *testing.T) {
	src := "package p\n\nimport . \"net/http\"\n\nvar _ = DefaultClient\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := implicitTransportUses(fset, f, "p.go"); len(got) != 1 || !strings.Contains(got[0], "dot-import") {
		t.Fatalf("scanner found %v, want one dot-import finding", got)
	}
}

// Only the two functions that build core's default transport may construct
// an http.Transport; another function in the same file may not.
func TestImplicitTransportUses_TransportAllowlistIsFileAndFunction(t *testing.T) {
	src := `package p

import "net/http"

func New() { _ = &http.Transport{} }

func newHCClient() { _ = &http.Transport{} }

func other() { _ = &http.Transport{} }
`
	cases := []struct {
		path string
		want int
	}{
		{"internal/proxy/server.go", 2},    // New allowed; newHCClient and other not
		{"internal/balancer/health.go", 2}, // newHCClient allowed; New and other not
		{"internal/proxy/elsewhere.go", 3}, // nothing allowed in another file
	}
	for _, tc := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, tc.path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if got := implicitTransportUses(fset, f, tc.path); len(got) != tc.want {
			t.Errorf("%s: scanner found %d use(s), want %d: %v", tc.path, len(got), tc.want, got)
		}
	}
}

// defaultTransportBuilders are the only functions (file, function name) that
// may construct an http.Transport: the server's default outbound transport,
// and the balancer's probe transport for a balancer built without one.
var defaultTransportBuilders = map[string]bool{
	"internal/proxy/server.go New":            true,
	"internal/balancer/health.go newHCClient": true,
}

// implicitTransportUses lists, as "file:line: reason", every place in f (at
// the module-relative, slash-separated path) that reaches Go's default
// transport or builds a transport of its own.
func implicitTransportUses(fset *token.FileSet, f *ast.File, path string) []string {
	var out []string
	report := func(n ast.Node, why string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+why)
	}
	httpNames, httputilNames := map[string]bool{}, map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		var set map[string]bool
		switch p {
		case "net/http":
			set = httpNames
		case "net/http/httputil":
			set = httputilNames
		default:
			continue
		}
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			report(imp, "dot-import of "+p+" hides its default client and transport from this check")
			continue
		}
		set[name] = true
	}
	if len(httpNames) == 0 && len(httputilNames) == 0 {
		return out
	}

	// sel reports whether e is pkg.name for one of the given package names.
	sel := func(e ast.Expr, pkgs map[string]bool, names ...string) (string, bool) {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		id, ok := s.X.(*ast.Ident)
		if !ok || !pkgs[id.Name] {
			return "", false
		}
		for _, n := range names {
			if s.Sel.Name == n {
				return n, true
			}
		}
		return "", false
	}
	hasKey := func(cl *ast.CompositeLit, key string) bool {
		for _, el := range cl.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
					return true
				}
			}
		}
		return false
	}

	var fn string // the enclosing top-level function, "" outside one
	visit := func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if name, ok := sel(n, httpNames, "DefaultClient", "DefaultTransport", "ProxyFromEnvironment"); ok {
				report(n, "http."+name+" bypasses the outbound transport")
			}
			if name, ok := sel(n, httputilNames, "NewSingleHostReverseProxy"); ok {
				report(n, "httputil."+name+" builds a ReverseProxy on the default transport")
			}
		case *ast.CallExpr:
			if name, ok := sel(n.Fun, httpNames, "Get", "Head", "Post", "PostForm"); ok {
				report(n, "http."+name+" uses the default client")
			}
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 {
				if _, ok := sel(n.Args[0], httpNames, "Client"); ok {
					report(n, "new(http.Client) has no Transport")
				}
				if _, ok := sel(n.Args[0], httpNames, "Transport"); ok && !defaultTransportBuilders[path+" "+fn] {
					report(n, "new(http.Transport) builds a transport a TransportFactory does not replace")
				}
			}
		case *ast.CompositeLit:
			if _, ok := sel(n.Type, httpNames, "Client"); ok && !hasKey(n, "Transport") {
				report(n, "http.Client literal without a Transport uses the default transport")
			}
			if _, ok := sel(n.Type, httpNames, "Transport"); ok && !defaultTransportBuilders[path+" "+fn] {
				report(n, "http.Transport literal builds a transport a TransportFactory does not replace")
			}
			if _, ok := sel(n.Type, httputilNames, "ReverseProxy"); ok && !hasKey(n, "Transport") {
				report(n, "httputil.ReverseProxy literal without a Transport uses the default transport")
			}
		case *ast.ValueSpec:
			if n.Type != nil && len(n.Values) == 0 {
				if _, ok := sel(n.Type, httpNames, "Client"); ok {
					report(n, "zero-value http.Client has no Transport")
				}
			}
		case *ast.StructType:
			for _, field := range n.Fields.List {
				if _, ok := sel(field.Type, httpNames, "Client"); ok {
					report(field, "value-type http.Client field starts with no Transport")
				}
			}
		}
		return true
	}
	for _, d := range f.Decls {
		fn = ""
		if fd, ok := d.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		ast.Inspect(d, visit)
	}
	return out
}

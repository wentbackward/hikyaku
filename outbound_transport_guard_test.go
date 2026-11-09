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
//   - an http.Client without a Transport (literal, new(http.Client) or a
//     zero-value variable);
//   - an httputil.ReverseProxy literal without a Transport, and
//     httputil.NewSingleHostReverseProxy.
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
		findings = append(findings, implicitTransportUses(fset, f)...)
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
)

var rt http.RoundTripper

`
	bad := []string{
		"_ = http.DefaultClient",
		"_ = nh.DefaultTransport",
		"_ = http.Transport{Proxy: http.ProxyFromEnvironment}",
		`_, _ = http.Get("u")`,
		`_, _ = nh.Head("u")`,
		`_, _ = http.Post("u", "", nil)`,
		`_, _ = http.PostForm("u", nil)`,
		"_ = &http.Client{Timeout: 1}",
		"_ = new(http.Client)",
		"var c http.Client; _ = c",
		"_ = &httputil.ReverseProxy{}",
		"_ = httputil.NewSingleHostReverseProxy(nil)",
	}
	good := []string{
		"_ = &http.Client{Transport: rt}",
		"_ = &nh.Client{Transport: rt, Timeout: 1}",
		"_ = &httputil.ReverseProxy{Transport: rt}",
		"var c *http.Client; _ = c",
		"_ = http.Transport{}",
	}
	scan := func(stmt string) []string {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", header+"func f() { "+stmt+" }\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return implicitTransportUses(fset, f)
	}
	for _, stmt := range bad {
		if got := scan(stmt); len(got) != 1 {
			t.Errorf("%s: scanner found %d use(s), want 1: %v", stmt, len(got), got)
		}
	}
	for _, stmt := range good {
		if got := scan(stmt); len(got) != 0 {
			t.Errorf("%s: scanner flagged an explicit-transport form: %v", stmt, got)
		}
	}
}

// implicitTransportUses lists, as "file:line: reason", every place in f that
// reaches Go's default transport.
func implicitTransportUses(fset *token.FileSet, f *ast.File) []string {
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
		set[name] = true
	}
	if len(httpNames) == 0 && len(httputilNames) == 0 {
		return nil
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

	var out []string
	report := func(n ast.Node, why string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+why)
	}
	ast.Inspect(f, func(n ast.Node) bool {
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
			}
		case *ast.CompositeLit:
			if _, ok := sel(n.Type, httpNames, "Client"); ok && !hasKey(n, "Transport") {
				report(n, "http.Client literal without a Transport uses the default transport")
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
		}
		return true
	})
	return out
}

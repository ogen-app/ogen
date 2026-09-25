package queues

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/riverqueue/river"
)

// TestEveryInsertQueueIsConfigured guards against a job that routes itself to a
// queue the River client doesn't run (CON-312): such a job is inserted fine and
// then never worked. It parses this package's sources, resolves the Queue of
// every river.InsertOpts literal, and checks each is in QueueConfigs.
func TestEveryInsertQueueIsConfigured(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}

	// Package-level string constants, so `Queue: AudioQueue` resolves.
	consts := map[string]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						consts[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
			return true
		})
	}

	configured := QueueConfigs(1, 1, 1)
	var seen int
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok || !isRiverInsertOpts(cl.Type) {
				return true
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Queue" {
					continue
				}
				seen++
				queue, ok := resolveQueue(kv.Value, consts)
				if !ok {
					t.Errorf("%s: can't resolve InsertOpts.Queue; use a package const", fset.Position(kv.Pos()))
					continue
				}
				if _, ok := configured[queue]; !ok {
					t.Errorf("%s: queue %q is not in QueueConfigs — its jobs would never run", fset.Position(kv.Pos()), queue)
				}
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("found no InsertOpts{Queue: ...} literals — the scan is broken")
	}
}

func isRiverInsertOpts(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "InsertOpts" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "river"
}

func resolveQueue(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "river" && v.Sel.Name == "QueueDefault" {
			return river.QueueDefault, true
		}
	}
	return "", false
}

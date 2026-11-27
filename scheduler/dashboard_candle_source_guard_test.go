package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestDashboardCandleSourceGuard(t *testing.T) {
	files, err := filepath.Glob("ui_candle_*.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"runPython": true, "runPythonReadOnly": true, "runPythonSideEffect": true, "spawnPythonProcess": true,
		"spawnPythonProcessRaw": true, "spawnPythonProcessWithEnv": true, "FetchUICandles": true, "hlInfoPost": true,
		"hlPostInfoTo": true, "fetchHyperliquid": true, "ApplyGeneration": true, "admitKeyFetch": true,
		"executeKeyFetch": true, "fetchIncremental": true, "recoverKeyOnce": true,
	}
	bannedHTTP := map[string]bool{"Client": true, "Get": true, "Post": true, "Head": true, "PostForm": true,
		"NewRequest": true, "NewRequestWithContext": true, "DefaultClient": true, "DefaultTransport": true, "Transport": true}
	checked := 0
	for _, path := range files {
		if filepath.Ext(path) != ".go" || len(path) > 8 && path[len(path)-8:] == "_test.go" {
			continue
		}
		checked++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if banned[x.Name] {
					t.Errorf("%s: dashboard candle code references %s; it must not reach a venue, the feed fetch path or Python", fset.Position(x.Pos()), x.Name)
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					if id.Name == "http" && bannedHTTP[x.Sel.Name] {
						t.Errorf("%s: dashboard candle code builds an HTTP client (http.%s)", fset.Position(x.Pos()), x.Sel.Name)
					}
					if id.Name == "ss" && x.Sel.Name == "mu" {
						t.Errorf("%s: dashboard candle code takes the trading state lock", fset.Position(x.Pos()))
					}
				}
				if inner, ok := x.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "ss" && x.Sel.Name == "mu" {
					t.Errorf("%s: dashboard candle code takes the trading state lock", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
	if checked < 6 {
		t.Fatalf("the guard checked %d dashboard candle files, want at least 6", checked)
	}
}

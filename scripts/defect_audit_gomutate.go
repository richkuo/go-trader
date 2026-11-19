package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type site struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"end_line"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Family   string `json:"family"`
	Op       string `json:"op"`
	Orig     string `json:"orig"`
	Repl     string `json:"repl"`
	Function string `json:"function"`
}

type walker struct {
	fset  *token.FileSet
	src   []byte
	rel   string
	sites []site
	funcs []*ast.FuncType
	names []string
	skip  map[ast.Node]bool
}

func (w *walker) add(from, to token.Pos, family, op, repl string) {
	s := w.fset.Position(from)
	e := w.fset.Position(to)
	orig := string(w.src[s.Offset:e.Offset])
	if orig == repl {
		return
	}
	fn := ""
	if len(w.names) > 0 {
		fn = w.names[len(w.names)-1]
	}
	w.sites = append(w.sites, site{File: w.rel, Line: s.Line, Col: s.Column, EndLine: e.Line, Start: s.Offset, End: e.Offset, Family: family, Op: op, Orig: orig, Repl: repl, Function: fn})
}

func (w *walker) text(n ast.Node) string {
	return string(w.src[w.fset.Position(n.Pos()).Offset:w.fset.Position(n.End()).Offset])
}

func isStringLit(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING || v.Kind == token.CHAR
	case *ast.BinaryExpr:
		return v.Op == token.ADD && (isStringLit(v.X) || isStringLit(v.Y))
	case *ast.ParenExpr:
		return isStringLit(v.X)
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "fmt" || id.Name == "strings" || id.Name == "filepath" || id.Name == "strconv") {
				return true
			}
		}
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "string" {
			return true
		}
	}
	return false
}

var boundary = map[token.Token]token.Token{token.LSS: token.LEQ, token.LEQ: token.LSS, token.GTR: token.GEQ, token.GEQ: token.GTR}
var negation = map[token.Token]token.Token{token.EQL: token.NEQ, token.NEQ: token.EQL}
var arith = map[token.Token]token.Token{token.ADD: token.SUB, token.SUB: token.ADD, token.MUL: token.QUO, token.QUO: token.MUL}
var arithAssign = map[token.Token]token.Token{token.ADD_ASSIGN: token.SUB_ASSIGN, token.SUB_ASSIGN: token.ADD_ASSIGN, token.MUL_ASSIGN: token.QUO_ASSIGN, token.QUO_ASSIGN: token.MUL_ASSIGN}

var numeric = map[string]bool{"int": true, "int8": true, "int16": true, "int32": true, "int64": true, "uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "float32": true, "float64": true, "byte": true, "rune": true, "uintptr": true, "time.Duration": true}

func (w *walker) resultTypes() []string {
	if len(w.funcs) == 0 {
		return nil
	}
	ft := w.funcs[len(w.funcs)-1]
	if ft == nil || ft.Results == nil {
		return nil
	}
	var out []string
	for _, f := range ft.Results.List {
		t := w.text(f.Type)
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, t)
		}
	}
	return out
}

func zeroFor(t, cur string) string {
	switch {
	case t == "bool":
		if cur == "true" {
			return "false"
		}
		if cur == "false" {
			return "true"
		}
		return "!(" + cur + ")"
	case t == "error":
		if cur == "nil" {
			return ""
		}
		return "nil"
	case t == "string":
		if cur == `""` {
			return ""
		}
		return `""`
	case numeric[t]:
		if cur == "0" {
			return "1"
		}
		return "0"
	}
	return ""
}

func (w *walker) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}
	if w.skip[n] {
		return nil
	}
	switch v := n.(type) {
	case *ast.GenDecl:
		if v.Tok == token.IMPORT {
			return nil
		}
	case *ast.ArrayType:
		if v.Len != nil {
			w.skip[v.Len] = true
		}
	case *ast.Field:
		if v.Tag != nil {
			w.skip[v.Tag] = true
		}
	case *ast.FuncDecl:
		w.funcs = append(w.funcs, v.Type)
		w.names = append(w.names, v.Name.Name)
		if v.Body != nil {
			ast.Walk(w, v.Body)
		}
		w.funcs = w.funcs[:len(w.funcs)-1]
		w.names = w.names[:len(w.names)-1]
		return nil
	case *ast.FuncLit:
		w.funcs = append(w.funcs, v.Type)
		name := "func literal"
		if len(w.names) > 0 {
			name = w.names[len(w.names)-1] + ".func"
		}
		w.names = append(w.names, name)
		ast.Walk(w, v.Body)
		w.funcs = w.funcs[:len(w.funcs)-1]
		w.names = w.names[:len(w.names)-1]
		return nil
	case *ast.IfStmt:
		w.add(v.Cond.Pos(), v.Cond.End(), "condition_negation", "negate_if", "!("+w.text(v.Cond)+")")
	case *ast.ForStmt:
		if v.Cond != nil {
			w.add(v.Cond.Pos(), v.Cond.End(), "condition_negation", "negate_for", "!("+w.text(v.Cond)+")")
		}
	case *ast.BinaryExpr:
		if r, ok := negation[v.Op]; ok {
			w.add(v.OpPos, v.OpPos+token.Pos(len(v.Op.String())), "condition_negation", v.Op.String()+"->"+r.String(), r.String())
		}
		if r, ok := boundary[v.Op]; ok {
			w.add(v.OpPos, v.OpPos+token.Pos(len(v.Op.String())), "comparison_boundary", v.Op.String()+"->"+r.String(), r.String())
		}
		if r, ok := arith[v.Op]; ok && !(v.Op == token.ADD && (isStringLit(v.X) || isStringLit(v.Y))) {
			w.add(v.OpPos, v.OpPos+token.Pos(len(v.Op.String())), "arithmetic_swap", v.Op.String()+"->"+r.String(), r.String())
		}
	case *ast.AssignStmt:
		if r, ok := arithAssign[v.Tok]; ok && !(v.Tok == token.ADD_ASSIGN && isStringLit(v.Rhs[0])) {
			w.add(v.TokPos, v.TokPos+token.Pos(len(v.Tok.String())), "arithmetic_swap", v.Tok.String()+"->"+r.String(), r.String())
		}
		if v.Tok != token.DEFINE {
			w.add(v.Pos(), v.End(), "statement_removal", "remove_assign", "_ = 0")
		}
	case *ast.IncDecStmt:
		w.add(v.Pos(), v.End(), "statement_removal", "remove_incdec", "_ = 0")
	case *ast.ExprStmt:
		if _, ok := v.X.(*ast.CallExpr); ok {
			w.add(v.Pos(), v.End(), "statement_removal", "remove_call", "_ = 0")
		}
	case *ast.BasicLit:
		switch v.Kind {
		case token.INT:
			n, err := strconv.ParseInt(strings.ReplaceAll(v.Value, "_", ""), 0, 64)
			if err == nil {
				repl := strconv.FormatInt(n+1, 10)
				if n == 1 {
					repl = "0"
				}
				w.add(v.Pos(), v.End(), "constant_change", "int", repl)
			}
		case token.FLOAT:
			f, err := strconv.ParseFloat(strings.ReplaceAll(v.Value, "_", ""), 64)
			if err == nil {
				repl := strconv.FormatFloat(f*2+1, 'g', -1, 64)
				if !strings.ContainsAny(repl, ".eE") {
					repl += ".0"
				}
				w.add(v.Pos(), v.End(), "constant_change", "float", repl)
			}
		}
	case *ast.Ident:
		if v.Name == "true" || v.Name == "false" {
			r := "false"
			if v.Name == "false" {
				r = "true"
			}
			w.add(v.Pos(), v.End(), "constant_change", "bool", r)
		}
	case *ast.ReturnStmt:
		types := w.resultTypes()
		if len(types) == len(v.Results) {
			for i, res := range v.Results {
				if repl := zeroFor(types[i], w.text(res)); repl != "" {
					w.add(res.Pos(), res.End(), "return_value", "return_"+types[i], repl)
					w.skip[res] = true
				}
			}
		}
	}
	return w
}

type guardReq struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Repl   string `json:"repl"`
	Family string `json:"family"`
	Op     string `json:"op"`
}

type guardRes struct {
	ID      string `json:"id"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Text    string `json:"text"`
	Kind    string `json:"kind"`
	Package string `json:"package"`
	Reason  string `json:"reason,omitempty"`
}

type parsed struct {
	fset *token.FileSet
	file *ast.File
	src  []byte
	tf   *token.File
}

const maxGuardBytes = 4000

func (p *parsed) span(n ast.Node) (int, int) {
	return p.tf.Offset(n.Pos()), p.tf.Offset(n.End())
}

func (p *parsed) path(start, end int) []ast.Node {
	var out []ast.Node
	var stack []ast.Node
	ast.Inspect(p.file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		s, e := p.span(n)
		if s <= start && end <= e {
			stack = append(stack, n)
			out = append(out[:0], stack...)
			return true
		}
		return false
	})
	return out
}

func blockList(parent ast.Node, child ast.Node) bool {
	var list []ast.Stmt
	switch v := parent.(type) {
	case *ast.BlockStmt:
		list = v.List
	case *ast.CaseClause:
		list = v.Body
	case *ast.CommClause:
		list = v.Body
	default:
		return false
	}
	for _, s := range list {
		if s == child {
			return true
		}
	}
	return false
}

func guardable(s ast.Stmt) bool {
	switch v := s.(type) {
	case *ast.AssignStmt:
		return v.Tok != token.DEFINE
	case *ast.DeclStmt, *ast.LabeledStmt, *ast.BranchStmt, *ast.EmptyStmt:
		return false
	}
	return true
}

func isBoolSite(r guardReq) bool {
	switch r.Family {
	case "condition_negation", "comparison_boundary":
		return true
	case "constant_change":
		return r.Op == "bool"
	case "return_value":
		return r.Op == "return_bool"
	}
	return false
}

func guard(p *parsed, r guardReq) guardRes {
	res := guardRes{ID: r.ID, Package: p.file.Name.Name}
	if r.Start < 0 || r.End > len(p.src) || r.Start > r.End {
		res.Reason = "site offsets out of range"
		return res
	}
	path := p.path(r.Start, r.End)
	if len(path) == 0 {
		res.Reason = "site not inside the file syntax tree"
		return res
	}
	mutate := func(s, e int) (string, string) {
		orig := string(p.src[s:e])
		mut := string(p.src[s:r.Start]) + r.Repl + string(p.src[r.End:e])
		return orig, mut
	}
	call := fmt.Sprintf("defectAuditOn(%q)", r.ID)
	if isBoolSite(r) {
		for i := len(path) - 1; i >= 0; i-- {
			if _, ok := path[i].(ast.Expr); !ok {
				continue
			}
			s, e := p.span(path[i])
			orig, mut := mutate(s, e)
			res.Start, res.End, res.Kind = s, e, "expr"
			res.Text = "(" + call + " && (" + mut + ") || !" + call + " && (" + orig + "))"
			if len(res.Text) > maxGuardBytes {
				res.Reason = "guard too large"
			}
			return res
		}
		res.Reason = "no enclosing expression"
		return res
	}
	for i := len(path) - 1; i >= 1; i-- {
		st, ok := path[i].(ast.Stmt)
		if !ok || !guardable(st) || !blockList(path[i-1], st) {
			continue
		}
		s, e := p.span(st)
		orig, mut := mutate(s, e)
		res.Start, res.End, res.Kind = s, e, "stmt"
		res.Text = "if " + call + " { " + mut + " } else { " + orig + " }"
		if len(res.Text) > maxGuardBytes {
			res.Reason = "guard too large"
		}
		return res
	}
	res.Reason = "no enclosing statement that can hold a guard"
	return res
}

func guards(root string) {
	var reqs []guardReq
	if err := json.NewDecoder(os.Stdin).Decode(&reqs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	files := map[string]*parsed{}
	var out []guardRes
	for _, r := range reqs {
		p, ok := files[r.File]
		if !ok {
			path := filepath.Join(root, r.File)
			src, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			p = &parsed{fset: fset, file: f, src: src, tf: fset.File(f.Pos())}
			files[r.File] = p
		}
		out = append(out, guard(p, r))
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-guards" {
		guards(os.Args[2])
		return
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: defect_audit_gomutate ROOT FILE... | defect_audit_gomutate -guards ROOT < sites.json")
		os.Exit(2)
	}
	root := os.Args[1]
	var all []site
	for _, rel := range os.Args[2:] {
		path := filepath.Join(root, rel)
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if ast.IsGenerated(f) {
			continue
		}
		w := &walker{fset: fset, src: src, rel: rel, skip: map[ast.Node]bool{}}
		for _, d := range f.Decls {
			ast.Walk(w, d)
		}
		all = append(all, w.sites...)
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(all); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

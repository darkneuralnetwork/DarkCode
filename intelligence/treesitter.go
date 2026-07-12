package intelligence

// treesitter.go — Go AST parser (the "tree-sitter" equivalent for Go).
//
// The spec names tree-sitter as the parsing backbone. For Go projects the
// stdlib go/ast gives exact structural information with no external
// dependency, so this is Go-first. The parser now extracts not just symbols
// (functions/types) but also call edges and type-embedding (inheritance) so
// the call + class graphs are populated during a scan.

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// ASTParser uses go/ast to extract symbols + graph edges from Go source.
type ASTParser struct {
	Fset *token.FileSet
}

func NewASTParser() *ASTParser {
	return &ASTParser{Fset: token.NewFileSet()}
}

// ParseResult is everything extracted from one Go file.
type ParseResult struct {
	Symbols []Symbol
	Imports []importEdge
	Calls   []CallEdge
	Embeds  []embedEdge
}

type importEdge struct {
	Path  string
	Alias string
}

type embedEdge struct {
	Type     string // the containing type name
	Embedded string // the embedded type name
}

// Parse extracts symbols, imports, calls, and embeddings from Go source.
func (p *ASTParser) Parse(code []byte, filePath string) (ParseResult, error) {
	file, err := parser.ParseFile(p.Fset, filePath, code, parser.ParseComments)
	if err != nil {
		return ParseResult{}, err
	}
	var r ParseResult

	// Imports.
	for _, imp := range file.Imports {
		path := imp.Path.Value
		// strip quotes
		if len(path) >= 2 {
			path = path[1 : len(path)-1]
		}
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		r.Imports = append(r.Imports, importEdge{Path: path, Alias: alias})
	}

	// Declarations, calls, and embeddings.
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			r.Symbols = append(r.Symbols, Symbol{
				Name: d.Name.Name, Kind: "function", FilePath: filePath,
				Line: p.Fset.Position(d.Pos()).Line, Receiver: receiverName(d),
			})
			// Collect call edges inside this function.
			caller := d.Name.Name
			if recv := receiverName(d); recv != "" {
				caller = stripPtr(recv) + "." + caller
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					r.Calls = append(r.Calls, CallEdge{
						Caller: caller,
						Callee: callName(call),
						File:   filePath,
						Line:   p.Fset.Position(call.Pos()).Line,
					})
				}
				return true
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				kind := "type"
				switch t := ts.Type.(type) {
				case *ast.StructType:
					kind = "struct"
					// Extract embedded fields (Go inheritance).
					if t.Fields != nil {
						for _, f := range t.Fields.List {
							for _, name := range embeddedNames(f.Type) {
								r.Embeds = append(r.Embeds, embedEdge{Type: ts.Name.Name, Embedded: name})
							}
						}
					}
				case *ast.InterfaceType:
					kind = "interface"
					if t.Methods != nil {
						for _, m := range t.Methods.List {
							for _, name := range embeddedNames(m.Type) {
								r.Embeds = append(r.Embeds, embedEdge{Type: ts.Name.Name, Embedded: name})
							}
						}
					}
				}
				r.Symbols = append(r.Symbols, Symbol{
					Name: ts.Name.Name, Kind: kind, FilePath: filePath,
					Line: p.Fset.Position(ts.Pos()).Line,
				})
			}
		}
	}
	return r, nil
}

func receiverName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	switch t := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "*" + id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func stripPtr(s string) string {
	if len(s) > 0 && s[0] == '*' {
		return s[1:]
	}
	return s
}

// callName extracts a best-effort callee identifier from a call expression.
func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name
		}
		return fn.Sel.Name
	}
	return "<expr>"
}

// embeddedNames returns the type names embedded in a field type (e.g. an
// anonymous *Foo field yields "Foo").
func embeddedNames(t ast.Expr) []string {
	switch e := t.(type) {
	case *ast.Ident:
		return []string{e.Name}
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return []string{id.Name}
		}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return []string{id.Name + "." + e.Sel.Name}
		}
	}
	return nil
}

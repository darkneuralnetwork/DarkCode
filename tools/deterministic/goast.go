package deterministic

// goast.go — Go AST helpers shared by definitions / imports / dependencies /
// rename. These give the deterministic toolchain real structural understanding
// of Go code (the spec's "AST / Language Server" priority) without an external
// LSP dependency.
//
// Multi-language note: for non-Go projects the tools fall back to ripgrep;
// the AST path is Go-first and extensible to tree-sitter later.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// goFile collects every .go file under root (non-recursive into vendor/).
type goFile struct {
	path string
	src  []byte
}

func collectGoFiles(root string) []goFile {
	var files []goFile
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip vendored / generated noise so the index reflects the project.
		if strings.Contains(path, "/vendor/") || strings.HasPrefix(path, "vendor/") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		files = append(files, goFile{path: path, src: data})
		return nil
	})
	return files
}

// definition is one symbol declaration found by the AST.
type definition struct {
	Name     string
	Kind     string // function | struct | interface | type | const | var
	File     string
	Line     int
	Receiver string // for methods: the receiver type name
}

// parseDefinitions extracts all top-level declarations from a Go file.
func parseDefinitions(fset *token.FileSet, f goFile) []definition {
	file, err := parser.ParseFile(fset, f.path, f.src, 0)
	if err != nil {
		return nil
	}
	var defs []definition
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			recv := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				if star, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
					recv = "*" + star.X.(*ast.Ident).Name
				} else if id, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
					recv = id.Name
				}
			}
			defs = append(defs, definition{
				Name: d.Name.Name, Kind: "function", File: f.path,
				Line: fset.Position(d.Pos()).Line, Receiver: recv,
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				kind := "type"
				switch ts.Type.(type) {
				case *ast.StructType:
					kind = "struct"
				case *ast.InterfaceType:
					kind = "interface"
				}
				defs = append(defs, definition{
					Name: ts.Name.Name, Kind: kind, File: f.path,
					Line: fset.Position(ts.Pos()).Line,
				})
			}
		}
	}
	return defs
}

// importEntry is one import declaration.
type importEntry struct {
	Path string // the import path string literal
	Alias string // alias, if any
	File  string
}

// parseImports extracts all import declarations from a Go file.
func parseImports(fset *token.FileSet, f goFile) []importEntry {
	file, err := parser.ParseFile(fset, f.path, f.src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var out []importEntry
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		out = append(out, importEntry{Path: p, Alias: alias, File: f.path})
	}
	return out
}

package intelligence

// langparse.go — symbol and edge extraction for the non-Go languages.
//
// Go is parsed exactly, by go/ast (treesitter.go). Everything else is scanned
// here with a per-language pattern table over source that has had comments and
// string literals blanked out first, which is what keeps the patterns from
// firing inside prose or quoted code.
//
// This is deliberately not a parser generator. Pulling in tree-sitter would
// mean CGo and a large native dependency tree, and the project's whole
// deployment story is one static binary with a dependency list you can audit
// in a minute. The graph does not need a parse tree — it needs four edge kinds
// (defines, imports, calls, extends), and those are recoverable from
// declaration syntax with good precision. Results are a superset-with-noise
// rather than ground truth: an unresolved call may be attributed to the
// nearest enclosing declaration, and dynamic constructs are invisible.

import (
	"path/filepath"
	"regexp"
	"strings"
)

// declPattern maps a declaration regex to the symbol kind it produces. The
// first capture group is the name.
type declPattern struct {
	kind string
	re   *regexp.Regexp
}

// langSpec describes how to scan one language.
type langSpec struct {
	lineComment string // "" when the language has none
	blockOpen   string
	blockClose  string
	decls       []declPattern
	imports     []*regexp.Regexp // first group: the imported module/path
	extends     []*regexp.Regexp // groups: child, then a comma-separated parent list
}

var langSpecs = map[string]langSpec{
	"typescript": {
		lineComment: "//", blockOpen: "/*", blockClose: "*/",
		decls: []declPattern{
			{"function", regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)},
			{"function", regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`)},
			{"class", regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)},
			{"interface", regexp.MustCompile(`(?m)^\s*(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`)},
			{"type", regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:type|enum)\s+([A-Za-z_$][\w$]*)`)},
			{"method", regexp.MustCompile(`(?m)^\s+(?:public\s+|private\s+|protected\s+|static\s+|readonly\s+|async\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::\s*[\w<>\[\]|\s,.]+)?\s*\{`)},
		},
		imports: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*import\s+(?:[^'"]*\s+from\s+)?['"]([^'"]+)['"]`),
			regexp.MustCompile(`(?m)^\s*(?:export\s+)?.*\brequire\s*\(\s*['"]([^'"]+)['"]\s*\)`),
		},
		extends: []*regexp.Regexp{
			regexp.MustCompile(`\bclass\s+([A-Za-z_$][\w$]*)\s+extends\s+([A-Za-z_$][\w$.]*)`),
			regexp.MustCompile(`\b(?:class|interface)\s+([A-Za-z_$][\w$]*)[^{]*?\bimplements\s+([^{]+)`),
			regexp.MustCompile(`\binterface\s+([A-Za-z_$][\w$]*)\s+extends\s+([^{]+)`),
		},
	},
	"python": {
		lineComment: "#",
		decls: []declPattern{
			{"function", regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`)},
			{"class", regexp.MustCompile(`(?m)^\s*class\s+([A-Za-z_]\w*)`)},
		},
		imports: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*from\s+([\w.]+)\s+import\b`),
			regexp.MustCompile(`(?m)^\s*import\s+([\w.]+)`),
		},
		extends: []*regexp.Regexp{
			regexp.MustCompile(`\bclass\s+([A-Za-z_]\w*)\s*\(([^)]+)\)`),
		},
	},
	"rust": {
		lineComment: "//", blockOpen: "/*", blockClose: "*/",
		decls: []declPattern{
			{"function", regexp.MustCompile(`(?m)^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+([A-Za-z_]\w*)`)},
			{"struct", regexp.MustCompile(`(?m)^\s*(?:pub(?:\([^)]*\))?\s+)?struct\s+([A-Za-z_]\w*)`)},
			{"interface", regexp.MustCompile(`(?m)^\s*(?:pub(?:\([^)]*\))?\s+)?trait\s+([A-Za-z_]\w*)`)},
			{"type", regexp.MustCompile(`(?m)^\s*(?:pub(?:\([^)]*\))?\s+)?(?:enum|type)\s+([A-Za-z_]\w*)`)},
		},
		imports: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*(?:pub\s+)?use\s+([\w:]+)`),
		},
		extends: []*regexp.Regexp{
			// `impl Trait for Type` is Rust's "Type implements Trait".
			regexp.MustCompile(`(?m)^\s*impl\s*(?:<[^>]*>)?\s+([A-Za-z_]\w*)(?:<[^>]*>)?\s+for\s+([A-Za-z_]\w*)`),
		},
	},
	"java": {
		lineComment: "//", blockOpen: "/*", blockClose: "*/",
		decls: []declPattern{
			{"class", regexp.MustCompile(`(?m)^\s*(?:public\s+|private\s+|protected\s+|abstract\s+|final\s+|static\s+)*class\s+([A-Za-z_$][\w$]*)`)},
			{"interface", regexp.MustCompile(`(?m)^\s*(?:public\s+|private\s+|protected\s+)*interface\s+([A-Za-z_$][\w$]*)`)},
			{"type", regexp.MustCompile(`(?m)^\s*(?:public\s+|private\s+|protected\s+)*(?:enum|record)\s+([A-Za-z_$][\w$]*)`)},
			{"method", regexp.MustCompile(`(?m)^\s+(?:public\s+|private\s+|protected\s+|static\s+|final\s+|synchronized\s+|abstract\s+|native\s+)+(?:<[^>]+>\s*)?[\w<>\[\],.\s]+\s+([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?:throws\s[\w,.\s]+)?\{`)},
		},
		imports: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^\s*import\s+(?:static\s+)?([\w.]+)`),
		},
		extends: []*regexp.Regexp{
			regexp.MustCompile(`\b(?:class|interface)\s+([A-Za-z_$][\w$]*)[^{]*?\bextends\s+([\w$.,<>\s]+?)(?:\bimplements\b|\{)`),
			regexp.MustCompile(`\bclass\s+([A-Za-z_$][\w$]*)[^{]*?\bimplements\s+([\w$.,<>\s]+?)\{`),
		},
	},
}

// extToLang maps a file extension to a spec key. JavaScript shares the
// TypeScript spec — the declaration syntax it uses is a subset.
var extToLang = map[string]string{
	".ts": "typescript", ".tsx": "typescript", ".mts": "typescript", ".cts": "typescript",
	".js": "typescript", ".jsx": "typescript", ".mjs": "typescript", ".cjs": "typescript",
	".py": "python", ".pyi": "python",
	".rs":   "rust",
	".java": "java",
}

// LanguageOf returns the language key for a path, or "" when unsupported.
// Go is reported as "go" but is parsed by the AST parser, not this scanner.
func LanguageOf(path string) string {
	if strings.HasSuffix(path, ".go") {
		return "go"
	}
	return extToLang[strings.ToLower(filepath.Ext(path))]
}

// callSite matches an identifier (optionally qualified) being invoked.
var callSite = regexp.MustCompile(`\b([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)?)\s*\(`)

// notCalls are control-flow and declaration keywords that are followed by a
// parenthesis but are not function calls.
var notCalls = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "with": true, "match": true, "def": true, "fn": true,
	"function": true, "class": true, "print": false, "super": true,
	"and": true, "or": true, "not": true, "in": true, "is": true,
	"elif": true, "except": true, "assert": true, "del": true, "lambda": true,
	"synchronized": true, "throw": true, "throws": true, "new": true,
	"await": true, "yield": true, "typeof": true, "instanceof": true, "sizeof": true,
	"do": true, "else": true, "try": true, "finally": true, "case": true,
}

// ParseText extracts symbols and edges from a non-Go source file. An
// unsupported extension yields an empty result rather than an error, so the
// indexer can call it for every file.
func ParseText(code []byte, path string) ParseResult {
	spec, ok := langSpecs[LanguageOf(path)]
	if !ok {
		return ParseResult{}
	}
	// Declarations, calls and inheritance are read from source with strings
	// blanked, so a quoted "class Foo" is never mistaken for one. Imports are
	// read with strings intact, because in JS/TS the module path *is* a string
	// literal. Both passes preserve byte offsets, so line numbers stay valid.
	src := stripNoise(string(code), spec, true)
	withStrings := stripNoise(string(code), spec, false)
	var r ParseResult

	// Declarations. A line offset table turns match positions into line numbers.
	lines := lineStarts(src)
	declLine := map[int]string{} // line → declared name, for attributing calls
	for _, d := range spec.decls {
		for _, m := range d.re.FindAllStringSubmatchIndex(src, -1) {
			name := src[m[2]:m[3]]
			if notCalls[name] {
				continue // a control-flow keyword, not a declaration
			}
			line := lineOf(lines, m[2])
			r.Symbols = append(r.Symbols, Symbol{
				Name: name, Kind: d.kind, FilePath: path, Line: line,
			})
			if d.kind == "function" || d.kind == "method" {
				declLine[line] = name
			}
		}
	}

	for _, re := range spec.imports {
		for _, m := range re.FindAllStringSubmatch(withStrings, -1) {
			if p := strings.TrimSpace(m[1]); p != "" {
				r.Imports = append(r.Imports, importEdge{Path: p})
			}
		}
	}

	for _, re := range spec.extends {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			for _, parent := range strings.Split(m[2], ",") {
				// Drop generic parameters and whitespace: "List<Foo>" → "List".
				if p, _, _ := strings.Cut(strings.TrimSpace(parent), "<"); p != "" {
					r.Embeds = append(r.Embeds, embedEdge{Type: m[1], Embedded: p})
				}
			}
		}
	}

	r.Calls = extractCalls(src, path, lines, declLine)
	return r
}

// extractCalls attributes each invocation to the nearest declaration above it.
// Without a parse tree that enclosing-scope guess is the honest best effort;
// it is right for the common one-function-per-block layout and degrades to
// "attributed to the previous function" otherwise.
func extractCalls(src, path string, lines []int, declLine map[int]string) []CallEdge {
	var calls []CallEdge
	caller := ""
	lastDecl := 0
	for _, m := range callSite.FindAllStringSubmatchIndex(src, -1) {
		line := lineOf(lines, m[2])
		// Adopt any declaration at or above this line as the current caller.
		for l := lastDecl + 1; l <= line; l++ {
			if name, ok := declLine[l]; ok {
				caller, lastDecl = name, l
			}
		}
		callee := src[m[2]:m[3]]
		if notCalls[callee] || callee == caller {
			continue
		}
		if declLine[line] == callee {
			continue // the declaration's own signature, not a call
		}
		calls = append(calls, CallEdge{Caller: caller, Callee: callee, File: path, Line: line})
	}
	return calls
}

// stripNoise blanks comments — and, when blankStrings is set, string literals
// too — while preserving every byte position and newline, so match offsets
// still map to the right line.
func stripNoise(src string, spec langSpec, blankStrings bool) string {
	out := []byte(src)
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	for i := 0; i < len(src); {
		switch {
		case spec.lineComment != "" && strings.HasPrefix(src[i:], spec.lineComment):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			blank(i, i+end)
			i += end
		case spec.blockOpen != "" && strings.HasPrefix(src[i:], spec.blockOpen):
			end := strings.Index(src[i+len(spec.blockOpen):], spec.blockClose)
			if end < 0 {
				blank(i, len(src))
				return string(out)
			}
			stop := i + len(spec.blockOpen) + end + len(spec.blockClose)
			blank(i, stop)
			i = stop
		case src[i] == '"' || src[i] == '\'' || src[i] == '`':
			quote := src[i]
			j := i + 1
			for j < len(src) && src[j] != quote {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if blankStrings {
				blank(i+1, min(j, len(src)))
			}
			i = j + 1
		default:
			i++
		}
	}
	return string(out)
}

// lineStarts records the byte offset of each line start.
func lineStarts(src string) []int {
	starts := []int{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineOf converts a byte offset to a 1-based line number by binary search.
func lineOf(starts []int, offset int) int {
	lo, hi := 0, len(starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if starts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

package memory

// kg_context.go — structural context compression.
//
// Every agent compresses context by summarising text. Having a code graph
// allows something better: replacing the text with its structure. A model does
// not need the body of a function to know that changing it breaks eleven
// callers — it needs the signature, the fan-in, and the dependency edges.
//
// So instead of a few thousand tokens of source, this emits a few hundred
// tokens of skeleton: which files matter for this goal, what they define, what
// depends on them, and how confident the graph is about each. Full source is
// then worth reading only for the two or three symbols actually being edited.
//
// The rendering is a dense line format rather than JSON. JSON spends a large
// fraction of its tokens on punctuation and repeated keys, which is exactly
// what this exists to avoid.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/darkcode/core"
)

// tokensPerChar is the usual ~4 chars/token rule for English and code. Used
// only to report savings, never to make a correctness decision.
const charsPerToken = 4

// Savings quantifies what the compression bought, so the cost claim is
// measured rather than asserted.
type Savings struct {
	Files        int `json:"files"`
	SourceTokens int `json:"source_tokens"` // estimated cost of inlining those files
	ViewTokens   int `json:"view_tokens"`   // what the skeleton actually costs
}

// Ratio is how many times smaller the skeleton is than the source. Returns 0
// when nothing was included.
func (s Savings) Ratio() float64 {
	if s.ViewTokens == 0 {
		return 0
	}
	return float64(s.SourceTokens) / float64(s.ViewTokens)
}

// fileRelevance scores one file against the goal's keywords.
type fileRelevance struct {
	label   string
	score   float64
	fanIn   int
	conf    float64
	symbols []string
	imports []string
}

// StructuralView renders the code relevant to a goal as a compact skeleton,
// fitted to a token budget, plus the savings it represents.
//
// Relevance is keyword overlap against file paths and the symbols they define,
// widened by graph centrality — a file half the repository depends on is worth
// mentioning even when the goal does not name it.
func (kg *KnowledgeGraph) StructuralView(goal string, budgetTokens int) (string, Savings) {
	if budgetTokens <= 0 {
		budgetTokens = 800
	}
	want := keywordsOf(goal)
	if len(want) == 0 {
		return "", Savings{}
	}

	candidates := kg.scoreFiles(want)
	if len(candidates) == 0 {
		return "", Savings{}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	var b strings.Builder
	b.WriteString("## Repository structure (compressed)\n")
	b.WriteString("Legend: `<-N` files depend on it · `->` its local imports · `?N` confidence when below 1.\n")
	b.WriteString("Read full source only for what you are editing; this is the shape of everything else.\n")

	var savings Savings
	for _, c := range candidates {
		entry := renderFile(c)
		if core.EstimateTokens(b.String())+core.EstimateTokens(entry) > budgetTokens {
			break
		}
		b.WriteString(entry)
		savings.Files++
		savings.SourceTokens += sourceTokens(c.label)
	}
	if savings.Files == 0 {
		return "", Savings{}
	}
	view := b.String()
	savings.ViewTokens = core.EstimateTokens(view)
	return view, savings
}

// scoreFiles ranks every indexed file against the goal's keywords.
func (kg *KnowledgeGraph) scoreFiles(want map[string]bool) []fileRelevance {
	var out []fileRelevance
	for _, n := range kg.FindByType(core.KGNodeFile) {
		if n.Properties["origin"] != "code_index" {
			continue
		}
		rel := fileRelevance{label: n.Label, conf: n.Confidence}

		// A path segment matching the goal is a strong signal.
		for _, part := range strings.FieldsFunc(strings.ToLower(n.Label), func(r rune) bool {
			return r == '/' || r == '.' || r == '_' || r == '-'
		}) {
			if want[part] {
				rel.score += 2
			}
		}

		for _, e := range kg.GetEdges(n.ID) {
			switch {
			case e.Relation == core.KGRelDefines && e.From == n.ID:
				sym, ok := kg.GetNode(e.To)
				if !ok {
					continue
				}
				kind := sym.Properties["kind"]
				refs, _ := strconv.Atoi(sym.Properties["references"])
				rel.fanIn += refs
				rel.symbols = append(rel.symbols, sym.Label+":"+shortKind(kind))
				if want[strings.ToLower(sym.Label)] {
					rel.score += 3 // the goal names this symbol
				}
			case e.Relation == core.KGRelImports && e.From == n.ID:
				rel.imports = append(rel.imports, strings.TrimPrefix(e.To, "package:"))
			}
		}
		if rel.score == 0 {
			continue // unrelated to the goal; centrality alone is not a reason to include it
		}
		// A test file is rarely the shape you need to understand production
		// code, and there are a lot of them — downweight rather than exclude,
		// so a goal that is genuinely about tests can still surface them.
		if isTestFile(rel.label) {
			rel.score *= 0.3
		}
		// Centrality is a tie-breaker, not a qualifier: it lifts a relevant
		// file that many others depend on above an equally relevant leaf.
		rel.score += float64(rel.fanIn) / 100
		sort.Strings(rel.symbols)
		sort.Strings(rel.imports)
		out = append(out, rel)
	}
	return out
}

// renderFile emits one file's skeleton in the dense line format.
func renderFile(c fileRelevance) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s", c.label)
	if c.fanIn > 0 {
		fmt.Fprintf(&b, " <-%d", c.fanIn)
	}
	if c.conf > 0 && c.conf < 1 {
		fmt.Fprintf(&b, " ?%.1f", c.conf)
	}
	b.WriteByte('\n')

	if len(c.symbols) > 0 {
		syms := c.symbols
		// A file with a hundred symbols would swamp the budget; the first
		// dozen convey its shape.
		if len(syms) > 12 {
			syms = append(syms[:12:12], fmt.Sprintf("…+%d", len(c.symbols)-12))
		}
		fmt.Fprintf(&b, "  %s\n", strings.Join(syms, " "))
	}
	if len(c.imports) > 0 {
		imps := c.imports
		if len(imps) > 8 {
			imps = imps[:8:8]
		}
		fmt.Fprintf(&b, "  -> %s\n", strings.Join(imps, " "))
	}
	return b.String()
}

// shortKind abbreviates a symbol kind so the skeleton stays dense.
func shortKind(kind string) string {
	switch kind {
	case "function":
		return "fn"
	case "method":
		return "m"
	case "struct":
		return "st"
	case "interface":
		return "if"
	case "type":
		return "ty"
	default:
		return "s"
	}
}

// sourceTokens estimates what inlining a file would have cost. Unreadable
// files contribute nothing rather than a guess.
func sourceTokens(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		if abs, aerr := filepath.Abs(path); aerr == nil {
			info, err = os.Stat(abs)
		}
		if err != nil {
			return 0
		}
	}
	return int(info.Size()) / charsPerToken
}

// keywordsOf reduces a goal to the meaningful lowercase words used for
// matching, splitting camelCase so "STMAdd" matches a goal mentioning "stm".
func keywordsOf(goal string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(goal, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
	}) {
		lower := strings.ToLower(w)
		if len(lower) > 2 && !isStopword(lower) {
			out[lower] = true
		}
		for _, part := range splitCamel(w) {
			if len(part) > 2 && !isStopword(part) {
				out[part] = true
			}
		}
	}
	return out
}

// splitCamel breaks camelCase, PascalCase and acronym-prefixed names into
// lowercase parts.
//
// The acronym case is the one that matters in Go: STMAdd, HTTPServer and
// URLPath all run two capitals together, so splitting only on lower→upper
// would leave them whole and a goal mentioning "add" or "server" would never
// match. The boundary is the last capital of a run, when a lowercase follows.
func splitCamel(s string) []string {
	upper := func(i int) bool { return i >= 0 && i < len(s) && s[i] >= 'A' && s[i] <= 'Z' }
	lower := func(i int) bool { return i >= 0 && i < len(s) && s[i] >= 'a' && s[i] <= 'z' }

	var parts []string
	start := 0
	for i := 1; i < len(s); i++ {
		boundary := upper(i) && lower(i-1) || // parseConfig → parse|Config
			upper(i) && upper(i-1) && lower(i+1) // STMAdd → STM|Add
		if boundary {
			parts = append(parts, strings.ToLower(s[start:i]))
			start = i
		}
	}
	if start < len(s) {
		parts = append(parts, strings.ToLower(s[start:]))
	}
	return parts
}

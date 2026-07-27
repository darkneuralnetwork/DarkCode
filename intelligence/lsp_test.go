package intelligence

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// LSP is 0-based; every API in this repo is 1-based. Getting that wrong is an
// off-by-one nobody notices until a definition lands on the wrong line.
func TestParseLocationsConvertsToOneBased(t *testing.T) {
	raw := json.RawMessage(`[{"uri":"file:///tmp/a.go","range":{"start":{"line":0,"character":0}}}]`)
	locs := parseLocations(raw)
	if len(locs) != 1 {
		t.Fatalf("got %d locations", len(locs))
	}
	if locs[0].Line != 1 || locs[0].Column != 1 {
		t.Errorf("line/col = %d/%d, want 1/1", locs[0].Line, locs[0].Column)
	}
	if locs[0].File != "/tmp/a.go" {
		t.Errorf("file = %q", locs[0].File)
	}
}

// Servers return a single Location, an array, or LocationLink — all legal.
func TestParseLocationsAcceptsEveryShape(t *testing.T) {
	single := json.RawMessage(`{"uri":"file:///a.go","range":{"start":{"line":4,"character":2}}}`)
	if locs := parseLocations(single); len(locs) != 1 || locs[0].Line != 5 {
		t.Errorf("single Location form: %+v", locs)
	}
	link := json.RawMessage(`[{"targetUri":"file:///b.go","targetSelectionRange":{"start":{"line":9,"character":0}}}]`)
	if locs := parseLocations(link); len(locs) != 1 || locs[0].File != "/b.go" || locs[0].Line != 10 {
		t.Errorf("LocationLink form: %+v", locs)
	}
	if locs := parseLocations(json.RawMessage(`null`)); len(locs) != 0 {
		t.Errorf("null result should yield nothing, got %+v", locs)
	}
}

func TestHoverTextFlattensEveryShape(t *testing.T) {
	cases := map[string]string{
		`"plain string"`:                         "plain string",
		`{"kind":"markdown","value":"# Title"}`:  "# Title",
		`[{"value":"first"},{"value":"second"}]`: "first\nsecond",
	}
	for raw, want := range cases {
		if got := hoverText(json.RawMessage(raw)); got != want {
			t.Errorf("hoverText(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestURIRoundTrip(t *testing.T) {
	for _, p := range []string{"/tmp/a.go", "/tmp/with space/b.go", "/tmp/plus+file.go"} {
		if got := uriToPath(pathToURI(p)); got != p {
			t.Errorf("round trip of %q gave %q", p, got)
		}
	}
	if !strings.HasPrefix(pathToURI("/x"), "file://") {
		t.Error("URI missing the file:// scheme")
	}
}

// A machine with no language server is the common case and must be silent and
// safe, never an error the agent has to reason about.
func TestMissingServerDegradesQuietly(t *testing.T) {
	c := NewLSPClient(t.TempDir())
	defer c.Shutdown()

	if c.Available() {
		t.Error("no server should be running before the first query")
	}
	// An unsupported extension can never have a server.
	if _, _, err := c.Definition("notes.md", 1, 1); err == nil {
		t.Error("expected errLSPUnavailable for an unsupported language")
	}
	if _, err := c.Hover("notes.md", 1, 1); err == nil {
		t.Error("expected errLSPUnavailable from Hover")
	}
	// Shutting down without ever starting anything must not panic.
	c.Shutdown()
}

// The real protocol path, exercised against gopls when it is installed.
func TestGoplsResolvesDefinitionAndDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module lsptest\n\ngo 1.24\n")
	write("lib.go", "package lsptest\n\n// Greet greets name.\nfunc Greet(name string) string {\n\treturn \"hi \" + name\n}\n")
	write("main.go", "package lsptest\n\nfunc use() string {\n\treturn Greet(\"world\")\n}\n")
	write("broken.go", "package lsptest\n\nfunc bad() int {\n\treturn \"not an int\"\n}\n")

	c := NewLSPClient(dir)
	defer c.Shutdown()

	file, line, err := c.Definition(filepath.Join(dir, "main.go"), 4, 9)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if filepath.Base(file) != "lib.go" || line != 4 {
		t.Errorf("Greet resolved to %s:%d, want lib.go:4", filepath.Base(file), line)
	}

	// The payoff over an AST scan: a real error from a real type checker.
	diags, err := c.Diagnostics(filepath.Join(dir, "broken.go"))
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("expected a type error in broken.go")
	}
	if diags[0].Severity != "error" || !strings.Contains(diags[0].Message, "int") {
		t.Errorf("unexpected diagnostic: %+v", diags[0])
	}

	hov, err := c.Hover(filepath.Join(dir, "main.go"), 4, 9)
	if err != nil || !strings.Contains(hov, "Greet") {
		t.Errorf("hover = %q, %v", hov, err)
	}

	refs, err := c.References(filepath.Join(dir, "lib.go"), 4, 6)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) < 2 {
		t.Errorf("got %d references, want the declaration and its use", len(refs))
	}
	if !c.Available() {
		t.Error("Available() should report the running server")
	}
}

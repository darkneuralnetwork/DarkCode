package intelligence

// lsp.go — a real Language Server Protocol client.
//
// The AST index answers structural questions across the whole repository and
// survives between sessions. It cannot answer the questions that need a type
// checker: what does this expression resolve to, what are the actual compile
// errors in this file, where is this *specific* overload defined. A language
// server answers exactly those, correctly, for any language that has one.
//
// The two are complements, not alternatives, so this never replaces the index
// — it is consulted for precision and every failure falls back to the AST
// path. A missing language server is a normal condition, not an error.
//
// Protocol: JSON-RPC 2.0 over stdio with LSP's Content-Length framing. No
// dependency; the wire format is a header, a blank line, and a JSON body.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/internal/jsonframe"
)

// lspRequestTimeout bounds a single request. A wedged language server must
// degrade to the AST path, never hang the agent.
const lspRequestTimeout = 10 * time.Second

// lspStartTimeout bounds the initialize handshake, which on a cold gopls
// includes loading the module graph.
const lspStartTimeout = 60 * time.Second

// serverCommands maps a language to the language server to launch. The first
// entry found on PATH wins, so a machine with either pyright or pylsp works.
var serverCommands = map[string][][]string{
	"go":         {{"gopls"}},
	"typescript": {{"typescript-language-server", "--stdio"}, {"vtsls", "--stdio"}},
	"python":     {{"pyright-langserver", "--stdio"}, {"pylsp"}, {"jedi-language-server"}},
	"rust":       {{"rust-analyzer"}},
	"java":       {{"jdtls"}},
}

// Diagnostic is one problem the language server reports in a file.
type Diagnostic struct {
	Line     int    `json:"line"` // 1-based, for humans
	Column   int    `json:"column"`
	Severity string `json:"severity"` // error | warning | info | hint
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"`
}

// Location is a resolved position in a file.
type Location struct {
	File   string `json:"file"`
	Line   int    `json:"line"` // 1-based
	Column int    `json:"column"`
}

// server is one running language server process.
type server struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nextID int64

	mu      sync.Mutex
	pending map[int64]chan json.RawMessage
	// diagnostics are pushed by the server unsolicited, keyed by file URI.
	diagnostics map[string][]Diagnostic
	closed      bool
}

// LSPClient manages one language server per language, started on demand.
type LSPClient struct {
	mu      sync.Mutex
	root    string
	servers map[string]*server
	// failed records languages whose server could not be started, so a missing
	// binary is not re-probed on every query.
	failed map[string]bool
}

// NewLSPClient returns a client rooted at the workspace. Servers are started
// lazily on first use, so constructing this is free.
func NewLSPClient(root string) *LSPClient {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &LSPClient{root: root, servers: map[string]*server{}, failed: map[string]bool{}}
}

// Available reports whether any language server is currently running.
func (c *LSPClient) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.servers) > 0
}

// Shutdown stops every running server.
func (c *LSPClient) Shutdown() {
	c.mu.Lock()
	servers := make([]*server, 0, len(c.servers))
	for _, s := range c.servers {
		servers = append(servers, s)
	}
	c.servers = map[string]*server{}
	c.mu.Unlock()

	for _, s := range servers {
		s.stop()
	}
}

// serverFor returns a running server for the file's language, starting one if
// needed. It returns errLSPUnavailable when the language has no configured
// server or the binary is not installed — the normal, expected case.
func (c *LSPClient) serverFor(file string) (*server, error) {
	lang := LanguageOf(file)
	if lang == "" {
		return nil, errLSPUnavailable
	}

	c.mu.Lock()
	if s, ok := c.servers[lang]; ok && !s.isClosed() {
		c.mu.Unlock()
		return s, nil
	}
	if c.failed[lang] {
		c.mu.Unlock()
		return nil, errLSPUnavailable
	}
	c.mu.Unlock()

	s, err := startServer(lang, c.root)
	if err != nil {
		c.mu.Lock()
		c.failed[lang] = true // don't re-probe a missing binary every query
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.servers[lang] = s
	c.mu.Unlock()
	return s, nil
}

// startServer launches and initializes a language server for one language.
func startServer(lang, root string) (*server, error) {
	argvs, ok := serverCommands[lang]
	if !ok {
		return nil, errLSPUnavailable
	}
	var argv []string
	for _, candidate := range argvs {
		if _, err := exec.LookPath(candidate[0]); err == nil {
			argv = candidate
			break
		}
	}
	if argv == nil {
		return nil, errLSPUnavailable
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // a language server's stderr is noise; diagnostics come over the protocol
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}

	s := &server{
		cmd: cmd, stdin: stdin,
		pending:     map[int64]chan json.RawMessage{},
		diagnostics: map[string][]Diagnostic{},
	}
	go s.readLoop(bufio.NewReaderSize(stdout, 64*1024))

	ctx, cancel := context.WithTimeout(context.Background(), lspStartTimeout)
	defer cancel()
	if _, err := s.call(ctx, "initialize", map[string]interface{}{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(root),
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"definition":         map[string]interface{}{},
				"hover":              map[string]interface{}{"contentFormat": []string{"plaintext", "markdown"}},
				"references":         map[string]interface{}{},
				"documentSymbol":     map[string]interface{}{},
				"publishDiagnostics": map[string]interface{}{},
			},
			"workspace": map[string]interface{}{"symbol": map[string]interface{}{}},
		},
	}); err != nil {
		s.stop()
		return nil, fmt.Errorf("initializing %s: %w", argv[0], err)
	}
	s.notify("initialized", map[string]interface{}{})
	return s, nil
}

// readLoop consumes framed messages, routing responses to their waiting caller
// and collecting the diagnostics the server pushes unsolicited.
func (s *server) readLoop(r *bufio.Reader) {
	defer s.markClosed()
	for {
		body, err := jsonframe.Read(r)
		if err != nil {
			return
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}

		// A notification (no id). Diagnostics are the ones worth keeping.
		if msg.ID == nil {
			if msg.Method == "textDocument/publishDiagnostics" {
				s.storeDiagnostics(msg.Params)
			}
			continue
		}
		// A request from the server to us: answer nothing useful, but do not
		// leave it hanging in a way that blocks the server's own progress.
		if msg.Method != "" {
			s.respondEmpty(*msg.ID)
			continue
		}

		s.mu.Lock()
		ch, ok := s.pending[*msg.ID]
		delete(s.pending, *msg.ID)
		s.mu.Unlock()
		if !ok {
			continue
		}
		if msg.Error != nil {
			ch <- nil // the caller reports a protocol error; nil is the signal
		} else {
			ch <- msg.Result
		}
	}
}

func (s *server) storeDiagnostics(params json.RawMessage) {
	var p struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
			} `json:"range"`
			Severity int    `json:"severity"`
			Message  string `json:"message"`
			Source   string `json:"source"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	out := make([]Diagnostic, 0, len(p.Diagnostics))
	for _, d := range p.Diagnostics {
		out = append(out, Diagnostic{
			// LSP positions are 0-based; humans and every other tool here are 1-based.
			Line:     d.Range.Start.Line + 1,
			Column:   d.Range.Start.Character + 1,
			Severity: severityName(d.Severity),
			Message:  d.Message,
			Source:   d.Source,
		})
	}
	s.mu.Lock()
	s.diagnostics[p.URI] = out
	s.mu.Unlock()
}

func severityName(n int) string {
	switch n {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	default:
		return "hint"
	}
}

// call sends a request and waits for its response.
func (s *server) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if s.isClosed() {
		return nil, errLSPUnavailable
	}
	id := atomic.AddInt64(&s.nextID, 1)
	ch := make(chan json.RawMessage, 1)

	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.write(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		if res == nil {
			return nil, fmt.Errorf("%s: language server returned an error", method)
		}
		return res, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *server) notify(method string, params interface{}) {
	_ = s.write(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *server) respondEmpty(id int64) {
	_ = s.write(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": nil})
}

// write frames and sends one message.
func (s *server) write(msg interface{}) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errLSPUnavailable
	}
	return jsonframe.Write(s.stdin, body)
}

func (s *server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *server) markClosed() {
	s.mu.Lock()
	s.closed = true
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
	s.mu.Unlock()
}

func (s *server) stop() {
	s.notify("shutdown", nil)
	s.markClosed()
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
}

// openFile tells the server about a file's current contents. LSP servers
// answer position queries against their own view of a document, so a file the
// server has never been told about yields nothing.
func (c *LSPClient) openFile(s *server, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lang := LanguageOf(file)
	s.notify("textDocument/didOpen", map[string]interface{}{
		"textDocument": map[string]interface{}{
			"uri": pathToURI(file), "languageId": lspLanguageID(lang),
			"version": 1, "text": string(data),
		},
	})
	return nil
}

// lspLanguageID maps our language key to the identifier LSP expects.
func lspLanguageID(lang string) string {
	switch lang {
	case "typescript":
		return "typescript"
	case "go", "python", "rust", "java":
		return lang
	}
	return lang
}

// Definition returns where the symbol at file:line:col is declared. Line and
// column are 1-based on the way in and out; LSP's 0-based positions are an
// implementation detail that stops here.
func (c *LSPClient) Definition(file string, line, col int) (string, int, error) {
	locs, err := c.locations(file, line, col, "textDocument/definition")
	if err != nil || len(locs) == 0 {
		if err == nil {
			err = errLSPUnavailable
		}
		return "", 0, err
	}
	return locs[0].File, locs[0].Line, nil
}

// References returns every use of the symbol at a position.
func (c *LSPClient) References(file string, line, col int) ([]Location, error) {
	return c.locations(file, line, col, "textDocument/references")
}

func (c *LSPClient) locations(file string, line, col int, method string) ([]Location, error) {
	s, err := c.serverFor(file)
	if err != nil {
		return nil, err
	}
	if err := c.openFile(s, file); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lspRequestTimeout)
	defer cancel()

	params := map[string]interface{}{
		"textDocument": map[string]interface{}{"uri": pathToURI(file)},
		"position":     map[string]interface{}{"line": line - 1, "character": col - 1},
	}
	if method == "textDocument/references" {
		params["context"] = map[string]interface{}{"includeDeclaration": true}
	}
	raw, err := s.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// parseLocations accepts every shape LSP allows for a location result: a
// single Location, an array of them, or an array of LocationLink.
func parseLocations(raw json.RawMessage) []Location {
	type lspRange struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	}
	type entry struct {
		URI         string   `json:"uri"`
		TargetURI   string   `json:"targetUri"`
		Range       lspRange `json:"range"`
		TargetRange lspRange `json:"targetSelectionRange"`
	}

	var entries []entry
	if json.Unmarshal(raw, &entries) != nil {
		var single entry
		if json.Unmarshal(raw, &single) != nil {
			return nil
		}
		entries = []entry{single}
	}

	var out []Location
	for _, e := range entries {
		uri, rng := e.URI, e.Range
		if uri == "" { // LocationLink form
			uri, rng = e.TargetURI, e.TargetRange
		}
		if uri == "" {
			continue
		}
		out = append(out, Location{
			File: uriToPath(uri), Line: rng.Start.Line + 1, Column: rng.Start.Character + 1,
		})
	}
	return out
}

// Hover returns the documentation and resolved type for a position — the
// answer to "what actually is this", which an AST cannot give.
func (c *LSPClient) Hover(file string, line, col int) (string, error) {
	s, err := c.serverFor(file)
	if err != nil {
		return "", err
	}
	if err := c.openFile(s, file); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lspRequestTimeout)
	defer cancel()

	raw, err := s.call(ctx, "textDocument/hover", map[string]interface{}{
		"textDocument": map[string]interface{}{"uri": pathToURI(file)},
		"position":     map[string]interface{}{"line": line - 1, "character": col - 1},
	})
	if err != nil {
		return "", err
	}
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(raw, &hover) != nil {
		return "", errLSPUnavailable
	}
	return hoverText(hover.Contents), nil
}

// hoverText flattens the three shapes LSP allows for hover contents.
func hoverText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var marked struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &marked) == nil && marked.Value != "" {
		return marked.Value
	}
	var list []struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &list) == nil {
		var b strings.Builder
		for _, m := range list {
			b.WriteString(m.Value)
			b.WriteByte('\n')
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// Diagnostics returns the problems the language server reports for a file:
// real type errors from a real type checker, which is precisely what the AST
// index cannot produce.
func (c *LSPClient) Diagnostics(file string) ([]Diagnostic, error) {
	s, err := c.serverFor(file)
	if err != nil {
		return nil, err
	}
	if err := c.openFile(s, file); err != nil {
		return nil, err
	}
	// Diagnostics arrive as an unsolicited notification after didOpen, so
	// there is nothing to wait on but the server's own analysis.
	uri := pathToURI(file)
	deadline := time.Now().Add(lspRequestTimeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		diags, ok := s.diagnostics[uri]
		s.mu.Unlock()
		if ok {
			return diags, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// No diagnostics published is a legitimate answer: the file is clean.
	return nil, nil
}

// WorkspaceSymbol searches every symbol the language server knows about,
// resolved rather than pattern-matched.
func (c *LSPClient) WorkspaceSymbol(query, anyFile string) ([]Location, error) {
	s, err := c.serverFor(anyFile)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lspRequestTimeout)
	defer cancel()

	raw, err := s.call(ctx, "workspace/symbol", map[string]interface{}{"query": query})
	if err != nil {
		return nil, err
	}
	var syms []struct {
		Name     string `json:"name"`
		Location struct {
			URI   string `json:"uri"`
			Range struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
			} `json:"range"`
		} `json:"location"`
	}
	if json.Unmarshal(raw, &syms) != nil {
		return nil, nil
	}
	out := make([]Location, 0, len(syms))
	for _, sym := range syms {
		out = append(out, Location{
			File:   uriToPath(sym.Location.URI),
			Line:   sym.Location.Range.Start.Line + 1,
			Column: sym.Location.Range.Start.Character + 1,
		})
	}
	return out, nil
}

// pathToURI converts an absolute path to a file:// URI.
func pathToURI(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return "file://" + (&url.URL{Path: filepath.ToSlash(p)}).EscapedPath()
}

// uriToPath converts a file:// URI back to a path.
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return u.Path
}

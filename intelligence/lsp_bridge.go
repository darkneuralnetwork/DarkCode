package intelligence

// LSPBridge connects to a language server for symbol / definition / hover
// queries. The NoOp implementation returns "not available" so callers fall
// back to the AST-backed path.
type LSPBridge interface {
	// Available reports whether a language server is connected.
	Available() bool
	// Definition returns the declaration site of a symbol at file:line.
	Definition(file string, line, col int) (string, int, error)
	// Hover returns hover documentation for a symbol at file:line.
	Hover(file string, line, col int) (string, error)
}

// NewLSPBridge returns the default LSP bridge (NoOp until a real LSP is wired).
func NewLSPBridge() LSPBridge { return &noOpLSP{} }

type noOpLSP struct{}

func (n *noOpLSP) Available() bool { return false }
func (n *noOpLSP) Definition(file string, line, col int) (string, int, error) {
	return "", 0, errLSPUnavailable
}
func (n *noOpLSP) Hover(file string, line, col int) (string, error) {
	return "", errLSPUnavailable
}

// lspError is a sentinel returned by the NoOp bridge.
type lspError string

func (e lspError) Error() string { return string(e) }

const errLSPUnavailable lspError = "no language server connected (using AST fallback)"

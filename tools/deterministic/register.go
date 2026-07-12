package deterministic

// register.go — registration entry point for the deterministic toolchain.
//
// The deterministic package imports github.com/darkcode/tools (for the
// ToolEntry / Registry types), so it cannot be registered from inside the
// tools package (import cycle). Instead the wiring layer (app_wireup.go)
// calls deterministic.RegisterAll(reg).

import "github.com/darkcode/tools"

// RegisterAll registers every deterministic tool into the given registry.
// These tools never invoke an LLM — they are backed by ripgrep + go/ast
// (spec §8: "Never use AI for: Rename Symbol, Find References, Imports,
// Definitions, Dependency Analysis").
func RegisterAll(reg *tools.Registry) {
	reg.Register(NewRenameTool())
	reg.Register(NewReferencesTool())
	reg.Register(NewImportsTool())
	reg.Register(NewDefinitionsTool())
	reg.Register(NewDependenciesTool())
}

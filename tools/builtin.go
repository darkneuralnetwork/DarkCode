package tools

import "github.com/darkcode/core"

// RegisterBuiltinTools registers all built-in tools into the given registry.
// This is called once at agent startup. Each tool gets its schema and handler
// wired up so the LLM can discover and call them.
func RegisterBuiltinTools(registry *Registry, memoryStore interface{}, router core.ModelRouter) {
	terminal := NewTerminalTool()
	fileTool := NewFileTool()
	searchTool := NewSearchTool()
	webTool := NewWebTool(registry, router)
	todoTool := NewTodoTool()
	gitTool := NewGitTool()
	monitorTool := NewMonitoringTool()

	// Terminal tool
	registry.Register(&ToolEntry{
		Name:        "terminal",
		Description: "Execute a shell command on the system. Returns stdout and stderr. Use for builds, git, scripts, package managers, and anything needing a shell.",
		Parameters:  MustParseSchema(terminal.Schema()),
		Handler:     terminal.Execute,
		Category:    "terminal",
	})

	// File read
	registry.Register(&ToolEntry{
		Name:        "read_file",
		Description: "Read a text file with line numbers. Use instead of cat/head/tail. Supports ~ for home directory.",
		Parameters:  MustParseSchema(fileTool.ReadSchema()),
		Handler:     fileTool.ReadFile,
		Category:    "file",
	})

	// List directory
	registry.Register(&ToolEntry{
		Name:        "list_dir",
		Description: "List the contents of a directory. Returns a list of files and subdirectories.",
		Parameters:  MustParseSchema(fileTool.ListDirSchema()),
		Handler:     fileTool.ListDir,
		Category:    "file",
	})

	// File write
	registry.Register(&ToolEntry{
		Name:        "write_file",
		Description: "Write content to a file, creating parent directories. Overwrites the entire file.",
		Parameters:  MustParseSchema(fileTool.WriteSchema()),
		Handler:     fileTool.WriteFile,
		Category:    "file",
	})

	// File patch (find-and-replace)
	registry.Register(&ToolEntry{
		Name:        "patch",
		Description: "Targeted find-and-replace edit in a file. Finds old_string and replaces with new_string. Use instead of sed.",
		Parameters:  MustParseSchema(fileTool.PatchSchema()),
		Handler:     fileTool.PatchFile,
		Category:    "file",
	})

	// Replace file content (multi-line chunk replace)
	registry.Register(&ToolEntry{
		Name:        "replace_file_content",
		Description: "Replaces a specific multi-line block of text with a new block of text. Best for targeted code edits.",
		Parameters:  MustParseSchema(fileTool.ReplaceSchema()),
		Handler:     fileTool.ReplaceFileContent,
		Category:    "file",
	})

	browserTool := NewBrowserTool()
	registry.Register(&ToolEntry{
		Name:        "browser_subagent",
		Description: "A headless browser subagent capable of visiting URLs, clicking elements, and typing text. Use this for complex web scraping or automated interactions.",
		Parameters:  MustParseSchema(browserTool.Schema()),
		Handler:     browserTool.Execute,
		Category:    "web",
	})

	// Search file contents
	registry.Register(&ToolEntry{
		Name:        "search_files",
		Description: "Search inside file contents using regex. Uses ripgrep if available, falls back to grep. Returns matching lines with line numbers.",
		Parameters:  MustParseSchema(searchTool.SearchSchema()),
		Handler:     searchTool.SearchContent,
		Category:    "file",
	})

	// List files
	registry.Register(&ToolEntry{
		Name:        "list_files",
		Description: "List files matching a glob pattern, sorted by modification time. Use instead of ls/find.",
		Parameters:  MustParseSchema(searchTool.ListSchema()),
		Handler:     searchTool.ListFiles,
		Category:    "file",
	})

	// Web fetch
	registry.Register(&ToolEntry{
		Name:        "web_fetch",
		Description: "Fetch content from a URL via HTTP GET. Returns the raw response body (up to 50KB).",
		Parameters:  MustParseSchema(webTool.FetchSchema()),
		Handler:     webTool.FetchURL,
		Category:    "web",
	})

	// Web search
	registry.Register(&ToolEntry{
		Name:        "web_search",
		Description: "Search the web using DuckDuckGo. Returns abstract text and related topics.",
		Parameters:  MustParseSchema(webTool.SearchSchema()),
		Handler:     webTool.WebSearch,
		Category:    "web",
	})

	// Todo list
	registry.Register(&ToolEntry{
		Name:        "todo",
		Description: "Manage the task list for the current session. Use for complex tasks with 3+ steps. Pass a 'todos' array to set/update items, or omit to read the current list.",
		Parameters:  MustParseSchema(todoTool.Schema()),
		Handler:     todoTool.Execute,
		Category:    "planning",
	})

	// Git tool
	registry.Register(&ToolEntry{
		Name:        "git",
		Description: "Execute git operations: status, diff, log, branch, add, commit, stash, show. Use for version control tasks.",
		Parameters:  MustParseSchema(gitTool.Schema()),
		Handler:     gitTool.Execute,
		Category:    "git",
	})

	// Monitoring tool
	registry.Register(&ToolEntry{
		Name:        "monitoring",
		Description: "System monitoring and health checks: system_info, processes, disk usage, health_check (URL), environment variables.",
		Parameters:  MustParseSchema(monitorTool.Schema()),
		Handler:     monitorTool.Execute,
		Category:    "monitoring",
	})

	// PDF tool — manipulate PDF files: info, extract_text, merge, split,
	// rotate. Pure-Go for info/extract_text; ghostscript-backed for
	// merge/split/rotate. Available in every tool-enabled mode (not General).
	registry.Register(&ToolEntry{
		Name:        "pdf",
		Description: "Manipulate PDF files. Operations: info (page count/version/metadata), extract_text (text from content streams), merge (concatenate PDFs), split (extract a page range), rotate (rotate pages). Args: operation, file (or files[] for merge), output (for merge/split/rotate), from/to (split), degrees (rotate).",
		Parameters: MustParseSchema(`{` +
			`"type":"object",` +
			`"properties":{` +
			`"operation":{"type":"string","enum":["info","extract_text","merge","split","rotate"],"description":"PDF operation to perform"},` +
			`"file":{"type":"string","description":"Path to the PDF file (or comma-separated list for merge)"},` +
			`"files":{"type":"array","items":{"type":"string"},"description":"List of PDF files to merge"},` +
			`"output":{"type":"string","description":"Output path for merge/split/rotate"},` +
			`"from":{"type":"integer","description":"First page (1-indexed) for split"},` +
			`"to":{"type":"integer","description":"Last page for split"},` +
			`"degrees":{"type":"integer","description":"Rotation degrees (90/180/270) for rotate"}` +
			`},` +
			`"required":["operation"]` +
			`}`),
		Handler:  pdfHandler,
		Category: "pdf",
	})

	// Image tool — manipulate image files: info, resize, convert.
	registry.Register(&ToolEntry{
		Name:        "image",
		Description: "Manipulate image files. Operations: info (dimensions, format), resize (width/height), convert (change format). Args: operation, file (input path), output (output path for resize/convert), width (resize), height (resize). Uses native Go or ImageMagick (convert) internally.",
		Parameters: MustParseSchema(`{` +
			`"type":"object",` +
			`"properties":{` +
			`"operation":{"type":"string","enum":["info","resize","convert"],"description":"Image operation to perform"},` +
			`"file":{"type":"string","description":"Path to the image file"},` +
			`"output":{"type":"string","description":"Output path for resize/convert"},` +
			`"width":{"type":"integer","description":"Width for resize (in pixels)"},` +
			`"height":{"type":"integer","description":"Height for resize (in pixels)"}` +
			`},` +
			`"required":["operation", "file"]` +
			`}`),
		Handler:  imageHandler,
		Category: "image",
	})

	// Memory tool (if memory store is provided)
	if store, ok := memoryStore.(interface {
		Add(string, string) (interface{}, error)
	}); ok {
		_ = store // store is used via the MemoryTool wrapper
	}
}

// NOTE: the deterministic toolchain (rename/references/imports/definitions/
// dependencies) is registered by deterministic.RegisterAll(reg) which is
// called from app_wireup.go. It cannot be registered here because the
// deterministic package imports this one (tools.ToolEntry), which would form
// an import cycle.

// RegisterMemoryTool registers the memory tool if a store is available.
func RegisterMemoryTool(registry *Registry, memTool *MemoryTool) {
	if memTool == nil {
		return
	}
	registry.Register(&ToolEntry{
		Name:        "memory",
		Description: "Save, update, remove, list, or search persistent memory entries that survive across sessions. Actions: add, replace, remove, list, search. Target: 'user' (profile) or 'memory' (notes).",
		Parameters:  MustParseSchema(memTool.Schema()),
		Handler:     memTool.Execute,
		Category:    "memory",
	})
}

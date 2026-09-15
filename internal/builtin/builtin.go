package builtin

import "github.com/zdaniels/fathom/internal/agent"

// All returns every starter tool. The agent factory uses this when
// opts.EnabledSkills is empty (default).
func All() []agent.ToolDefinition {
	out := []agent.ToolDefinition{}
	out = append(out, NotesTools()...)
	out = append(out, WebSearchTool())
	out = append(out, FileEditorTools()...)
	out = append(out, EditFileTool())
	out = append(out, GrepTool())
	out = append(out, GlobTool())
	out = append(out, ShellTool())
	out = append(out, PythonExecTool())
	out = append(out, ImageGenerateTool())
	out = append(out, SystemRunTool())
	return out
}

// ReadOnlyTools returns a curated read-only starter set: web search,
// grep, glob, and the non-mutating file tools (read_file, list_files —
// write_file and edit_file are excluded). This is the default tool set
// for delegated sub-agents, which should research/read without write or
// shell access unless the parent explicitly grants more.
func ReadOnlyTools() []agent.ToolDefinition {
	out := []agent.ToolDefinition{WebSearchTool(), GrepTool(), GlobTool()}
	for _, t := range FileEditorTools() {
		if t.Name == "read_file" || t.Name == "list_files" {
			out = append(out, t)
		}
	}
	return out
}

// ForSkills returns the subset of starter tools belonging to the named
// skills. Unknown names are silently skipped. The recognised names —
// usable in fathom.config.yaml's routing.profiles[].builtins list — are:
//
//	notes              create_note + search_notes
//	web-search         web_search
//	file-editor        read_file, write_file, list_files, edit_file, grep, glob
//	shell              bash
//	python             python_exec   (requires Docker)
//	image-generation   image_generate
//	system             system_run    (macOS AppleScript / system-level actions)
func ForSkills(skills []string) []agent.ToolDefinition {
	set := map[string]bool{}
	for _, s := range skills {
		set[s] = true
	}
	var out []agent.ToolDefinition
	if set["notes"] {
		out = append(out, NotesTools()...)
	}
	if set["web-search"] {
		out = append(out, WebSearchTool())
	}
	if set["file-editor"] {
		out = append(out, FileEditorTools()...)
		out = append(out, EditFileTool())
		out = append(out, GrepTool())
		out = append(out, GlobTool())
	}
	if set["shell"] {
		out = append(out, ShellTool())
	}
	if set["python"] {
		out = append(out, PythonExecTool())
	}
	if set["image-generation"] {
		out = append(out, ImageGenerateTool())
	}
	if set["system"] {
		out = append(out, SystemRunTool())
	}
	return out
}

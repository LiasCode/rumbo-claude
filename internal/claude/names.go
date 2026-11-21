package claude

import (
	"rumbo-claude/protocol"
	"strings"
)

// Match pi's canonical Claude Code tool casing on the wire only. The host
// always receives the original declared name, including custom tools.
func toClaudeName(name string) string {
	for _, canonical := range []string{"Read", "Write", "Edit", "Bash", "Grep", "Glob", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode", "KillShell", "NotebookEdit", "Skill", "Task", "TaskOutput", "TodoWrite", "WebFetch", "WebSearch"} {
		if strings.EqualFold(name, canonical) {
			return canonical
		}
	}
	return name
}

func fromClaudeName(name string, tools []protocol.ToolDef) string {
	for _, tool := range tools {
		if strings.EqualFold(name, tool.Name) {
			return tool.Name
		}
	}
	return name
}

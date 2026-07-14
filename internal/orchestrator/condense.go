package orchestrator

import (
	"strings"

	"github.com/xoralis/cocoder/internal/adapter"
)

const maxSummaryRunes = 2000

// condenseResult produces the prompt-ready summary of a finished task,
// walking down a ladder of fallbacks so dependents always get something
// useful on the blackboard:
//
//  1. the CLI's own result summary (claude result / codex last message)
//  2. otherwise the tail of the agent's visible text
//  3. otherwise a synthetic note listing the changed files
func condenseResult(res *adapter.TaskResult, agentText string, changed []string) string {
	if s := strings.TrimSpace(res.Summary); s != "" {
		return truncateTail(s, maxSummaryRunes)
	}
	if s := strings.TrimSpace(agentText); s != "" {
		return truncateTail(s, maxSummaryRunes)
	}
	if len(changed) > 0 {
		return "completed; changed files: " + strings.Join(changed, ", ")
	}
	return "completed (no summary reported)"
}

// truncateTail keeps the LAST n runes (the conclusion of a reply matters
// more than its preamble), prefixing an ellipsis when it trims.
func truncateTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "..." + string(r[len(r)-n:])
}

// clip limits s to n runes from the front (for large context like the
// architecture doc).
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n...(truncated)"
}

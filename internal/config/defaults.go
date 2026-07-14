package config

// builtinCLIs is the single place where external CLI knowledge is hardcoded
// (verified against official docs 2026-07). Every field can be overridden
// from ccd.yaml's `clis:` section, so interface drift is a config fix,
// not a code fix.
//
//	claude  claude -p --output-format stream-json --verbose   (prompt via stdin)
//	codex   codex exec --json ... -                           (prompt via stdin, "-")
//	gemini  gemini --output-format stream-json                (prompt via stdin pipe)
//	grok    community superagent-ai/grok-cli; generic text fallback
//	agy     Google Antigravity; headless interface UNVERIFIED, generic fallback
func builtinCLIs() map[string]*CLISpec {
	return map[string]*CLISpec{
		"claude": {
			Adapter:     "claude",
			Command:     "claude",
			Output:      "jsonl",
			PromptVia:   "stdin",
			VersionArgs: []string{"--version"},
		},
		"codex": {
			Adapter:     "codex",
			Command:     "codex",
			Output:      "jsonl",
			PromptVia:   "stdin",
			VersionArgs: []string{"--version"},
		},
		"gemini": {
			Adapter:     "gemini",
			Command:     "gemini",
			Output:      "jsonl",
			PromptVia:   "stdin",
			VersionArgs: []string{"--version"},
		},
		"grok": {
			Adapter:     "generic",
			Command:     "grok",
			RunArgs:     []string{"-p", "{prompt}"},
			PromptVia:   "arg",
			Output:      "text",
			VersionArgs: []string{"--version"},
		},
		"agy": {
			Adapter:     "generic",
			Command:     "agy",
			RunArgs:     []string{"{prompt}"},
			PromptVia:   "arg",
			Output:      "text",
			VersionArgs: []string{"--version"},
			Unverified:  true,
		},
	}
}

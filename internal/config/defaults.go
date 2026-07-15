package config

// builtinCLIs is the single place where external CLI knowledge is hardcoded
// (verified against installed CLIs 2026-07: claude 2.1, codex 0.144,
// grok 0.2.93; gemini per docs; agy unverified). Every field can be
// overridden from ccd.yaml's `clis:` section, so interface drift is a
// config fix, not a code fix.
//
//	claude  claude -p --output-format stream-json --verbose      (prompt via stdin)
//	codex   codex exec --json --skip-git-repo-check ... -         (prompt via stdin, "-")
//	grok    grok --output-format streaming-json --prompt-file <f> (prompt via file)
//	gemini  gemini --output-format stream-json                    (prompt via stdin; docs, unverified stream)
//	agy     Google Antigravity; headless interface UNVERIFIED, generic text fallback
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
			Adapter:     "grok",
			Command:     "grok",
			Output:      "jsonl",
			PromptVia:   "file",
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

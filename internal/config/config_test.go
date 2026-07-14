package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ccd.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shipped template must always load and validate.
func TestTemplateValidates(t *testing.T) {
	cfg, err := Load(writeTemp(t, TemplateYAML()))
	if err != nil {
		t.Fatalf("template config invalid: %v", err)
	}
	if len(cfg.Roles) == 0 {
		t.Fatal("template has no roles")
	}
	arch, ok := cfg.Roles["architect"]
	if !ok {
		t.Fatal("template missing architect role")
	}
	if arch.Permission != PermReadOnly {
		t.Errorf("architect permission = %q, want read-only", arch.Permission)
	}
	spec, err := cfg.ResolveCLI("claude")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Adapter != "claude" || spec.Command != "claude" || spec.PromptVia != "stdin" {
		t.Errorf("resolved claude spec = %+v", spec)
	}
	if cfg.Defaults.TaskTimeout.D() != 30*time.Minute {
		t.Errorf("task_timeout = %v", cfg.Defaults.TaskTimeout.D())
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Load(writeTemp(t, "version: 1\nbogus_key: 1\nroles:\n  dev: {cli: claude}\n"))
	if err == nil || !strings.Contains(err.Error(), "bogus_key") {
		t.Errorf("expected strict-mode error mentioning bogus_key, got %v", err)
	}
}

func TestBadPermissionRejected(t *testing.T) {
	_, err := Load(writeTemp(t, "version: 1\nroles:\n  dev: {cli: claude, permission: sudo}\n"))
	if err == nil || !strings.Contains(err.Error(), "permission") {
		t.Errorf("expected permission error, got %v", err)
	}
}

func TestUnknownCLIRejected(t *testing.T) {
	_, err := Load(writeTemp(t, "version: 1\nroles:\n  dev: {cli: nonexistent-tool}\n"))
	if err == nil || !strings.Contains(err.Error(), "nonexistent-tool") {
		t.Errorf("expected unknown-cli error, got %v", err)
	}
}

func TestNoRolesRejected(t *testing.T) {
	_, err := Load(writeTemp(t, "version: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Errorf("expected no-roles error, got %v", err)
	}
}

func TestPermissionDefaultsToEdits(t *testing.T) {
	cfg, err := Load(writeTemp(t, "version: 1\nroles:\n  dev: {cli: claude}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles["dev"].Permission != PermEdits {
		t.Errorf("permission = %q, want edits default", cfg.Roles["dev"].Permission)
	}
}

func TestGenericCLIViaConfig(t *testing.T) {
	cfg, err := Load(writeTemp(t, `version: 1
roles:
  dev: {cli: mycli}
clis:
  mycli:
    adapter: generic
    command: my-cli-bin
    run_args: ["-p", "{prompt}"]
    prompt_via: arg
    output: text
`))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := cfg.ResolveCLI("mycli")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Adapter != "generic" || spec.Command != "my-cli-bin" || spec.PromptVia != "arg" {
		t.Errorf("spec = %+v", spec)
	}
}

func TestBuiltinOverride(t *testing.T) {
	cfg, err := Load(writeTemp(t, `version: 1
roles:
  dev: {cli: claude}
clis:
  claude:
    command: claude-nightly
    extra_args: ["--fallback-model", "sonnet"]
`))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := cfg.ResolveCLI("claude")
	if spec.Command != "claude-nightly" {
		t.Errorf("command = %q", spec.Command)
	}
	if spec.Adapter != "claude" { // builtin adapter kind preserved
		t.Errorf("adapter = %q", spec.Adapter)
	}
	if len(spec.ExtraArgs) != 2 {
		t.Errorf("extra_args = %v", spec.ExtraArgs)
	}
}

func TestBadDurationRejected(t *testing.T) {
	_, err := Load(writeTemp(t, "version: 1\ndefaults: {task_timeout: soon}\nroles:\n  dev: {cli: claude}\n"))
	if err == nil || !strings.Contains(err.Error(), "duration") {
		t.Errorf("expected duration error, got %v", err)
	}
}

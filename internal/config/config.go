// Package config defines the ccd.yaml schema, loading and validation.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Permission is the coarse three-level autonomy grant for a role.
// Each adapter maps it to CLI-specific flags (see defaults.go / adapters).
type Permission string

const (
	PermReadOnly Permission = "read-only" // read/search only, no writes
	PermEdits    Permission = "edits"     // auto-approve file edits, no command execution
	PermFull     Permission = "full"      // fully autonomous (dangerous outside trusted repos)
)

// Duration wraps time.Duration so YAML accepts "30m" style strings.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30m\": %w", err)
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the root of ccd.yaml.
type Config struct {
	Version  int                 `yaml:"version"`
	Defaults Defaults            `yaml:"defaults"`
	Roles    map[string]*Role    `yaml:"roles"`
	CLIs     map[string]*CLISpec `yaml:"clis"`
}

// Defaults holds run-wide tunables.
type Defaults struct {
	TaskTimeout    Duration `yaml:"task_timeout"`    // per-task timeout
	Retries        int      `yaml:"retries"`         // task failure retries
	PlanRetries    int      `yaml:"plan_retries"`    // planner JSON validation retries
	BudgetUSD      float64  `yaml:"budget_usd"`      // per-run cost cap, 0 = unlimited
	BuilderRole    string   `yaml:"builder_role"`    // role used by --degrade single-task fallback
	ScopeViolation string   `yaml:"scope_violation"` // warn | fail
}

// Role maps a named role (architect/frontend/...) to a CLI and its settings.
type Role struct {
	CLI          string     `yaml:"cli"`
	Model        string     `yaml:"model"`
	Permission   Permission `yaml:"permission"`
	SystemPrompt string     `yaml:"system_prompt"`
	FileScope    []string   `yaml:"file_scope"`
	Timeout      Duration   `yaml:"timeout"`
	Retries      *int       `yaml:"retries"`
	ExtraArgs    []string   `yaml:"extra_args"`
}

// CLISpec describes how to drive one CLI. Built-in facts live in defaults.go
// and every field can be overridden from ccd.yaml (the drift mitigation).
type CLISpec struct {
	Name    string `yaml:"-"`
	Adapter string `yaml:"adapter"` // "" = builtin same-name adapter; or "generic"
	Command string `yaml:"command"` // binary name, resolved via PATH (finds .cmd shims on Windows)

	// Generic-adapter fields.
	RunArgs   []string `yaml:"run_args"`   // arg template; {prompt} {model} {workdir} {session} placeholders
	PromptVia string   `yaml:"prompt_via"` // stdin | arg
	Output    string   `yaml:"output"`     // jsonl | text

	VersionArgs []string `yaml:"version_args"`
	ExtraArgs   []string `yaml:"extra_args"`
	Bare        bool     `yaml:"bare"` // claude: add --bare (requires ANTHROPIC_API_KEY auth)

	// PermissionArgs overrides the builtin permission->flags mapping,
	// keyed by "read-only"/"edits"/"full".
	PermissionArgs map[string][]string `yaml:"permission_args"`

	Unverified bool `yaml:"-"` // builtin flag: headless interface not yet verified (agy)
}

// Load reads, decodes (strict), normalizes and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}
	return cfg, nil
}

// DefaultConfig returns the built-in defaults applied before file overlay.
func DefaultConfig() *Config {
	return &Config{
		Version: 1,
		Defaults: Defaults{
			TaskTimeout:    Duration(30 * time.Minute),
			Retries:        1,
			PlanRetries:    2,
			ScopeViolation: "warn",
		},
	}
}

func (c *Config) normalize() {
	for _, r := range c.Roles {
		if r == nil {
			continue
		}
		if r.Permission == "" {
			r.Permission = PermEdits
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.Version != 1 {
		errs = append(errs, fmt.Errorf("version must be 1, got %d", c.Version))
	}
	if len(c.Roles) == 0 {
		errs = append(errs, errors.New("at least one role must be defined under 'roles'"))
	}
	for _, name := range c.RoleNames() {
		r := c.Roles[name]
		if r == nil {
			errs = append(errs, fmt.Errorf("roles.%s: empty role definition", name))
			continue
		}
		if r.CLI == "" {
			errs = append(errs, fmt.Errorf("roles.%s.cli is required", name))
			continue
		}
		switch r.Permission {
		case PermReadOnly, PermEdits, PermFull:
		default:
			errs = append(errs, fmt.Errorf("roles.%s.permission must be one of read-only|edits|full, got %q", name, r.Permission))
		}
		if _, err := c.ResolveCLI(r.CLI); err != nil {
			errs = append(errs, fmt.Errorf("roles.%s: %w", name, err))
		}
	}
	if br := c.Defaults.BuilderRole; br != "" {
		if _, ok := c.Roles[br]; !ok {
			errs = append(errs, fmt.Errorf("defaults.builder_role %q is not a defined role", br))
		}
	}
	switch c.Defaults.ScopeViolation {
	case "", "warn", "fail":
	default:
		errs = append(errs, fmt.Errorf("defaults.scope_violation must be warn or fail, got %q", c.Defaults.ScopeViolation))
	}
	return errors.Join(errs...)
}

// RoleNames returns role names sorted for stable iteration.
func (c *Config) RoleNames() []string {
	names := make([]string, 0, len(c.Roles))
	for n := range c.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// CLINames returns the sorted set of CLI names referenced by roles plus
// explicitly defined ones.
func (c *Config) CLINames() []string {
	set := map[string]bool{}
	for _, r := range c.Roles {
		if r != nil && r.CLI != "" {
			set[r.CLI] = true
		}
	}
	for n := range c.CLIs {
		set[n] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResolveCLI merges the builtin spec (if any) with the user overlay from
// ccd.yaml and fills fallback defaults.
func (c *Config) ResolveCLI(name string) (*CLISpec, error) {
	var spec CLISpec
	base, isBuiltin := builtinCLIs()[name]
	if isBuiltin {
		spec = *base
	} else {
		spec = CLISpec{Adapter: "generic"}
	}
	user, hasUser := c.CLIs[name]
	if hasUser && user != nil {
		overlaySpec(&spec, user)
	}
	if !isBuiltin && (!hasUser || user == nil) {
		return nil, fmt.Errorf("cli %q is not built in and not defined under 'clis'", name)
	}
	spec.Name = name
	if spec.Command == "" {
		spec.Command = name
	}
	if spec.PromptVia == "" {
		spec.PromptVia = "stdin"
	}
	if spec.Output == "" {
		spec.Output = "text"
	}
	if len(spec.VersionArgs) == 0 {
		spec.VersionArgs = []string{"--version"}
	}
	return &spec, nil
}

func overlaySpec(dst, src *CLISpec) {
	if src.Adapter != "" {
		dst.Adapter = src.Adapter
	}
	if src.Command != "" {
		dst.Command = src.Command
	}
	if len(src.RunArgs) > 0 {
		dst.RunArgs = src.RunArgs
	}
	if src.PromptVia != "" {
		dst.PromptVia = src.PromptVia
	}
	if src.Output != "" {
		dst.Output = src.Output
	}
	if len(src.VersionArgs) > 0 {
		dst.VersionArgs = src.VersionArgs
	}
	if len(src.ExtraArgs) > 0 {
		dst.ExtraArgs = src.ExtraArgs
	}
	if src.Bare {
		dst.Bare = true
	}
	if len(src.PermissionArgs) > 0 {
		dst.PermissionArgs = src.PermissionArgs
	}
}

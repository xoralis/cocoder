package config

import _ "embed"

//go:embed ccd.template.yaml
var templateYAML string

// TemplateYAML returns the commented ccd.yaml template written by `ccd init`.
func TemplateYAML() string { return templateYAML }

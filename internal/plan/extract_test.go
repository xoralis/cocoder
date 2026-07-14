package plan

import (
	"strings"
	"testing"
)

func TestExtractJSONFence(t *testing.T) {
	text := "# Architecture\nUse a small HTTP server.\n\n```json\n{\"goal\":\"g\",\"tasks\":[{\"id\":\"t1\"}]}\n```\n"
	j, before, err := ExtractJSON(text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j, `"tasks"`) || strings.Contains(j, "```") {
		t.Errorf("json = %q", j)
	}
	if !strings.Contains(before, "# Architecture") || strings.Contains(before, "```") {
		t.Errorf("before = %q", before)
	}
}

func TestExtractJSONLastFenceWins(t *testing.T) {
	text := "example:\n```json\n{\"tasks\":[{\"id\":\"WRONG\"}]}\n```\nfinal:\n```json\n{\"tasks\":[{\"id\":\"RIGHT\"}]}\n```\n"
	j, _, err := ExtractJSON(text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j, "RIGHT") || strings.Contains(j, "WRONG") {
		t.Errorf("json = %q", j)
	}
}

func TestExtractJSONBareObjectFallback(t *testing.T) {
	text := "Here is the plan you asked for.\n\n{\"goal\":\"g\",\"tasks\":[{\"id\":\"t1\",\"title\":\"a {weird} title\"}]}\n\nGood luck!"
	j, before, err := ExtractJSON(text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j, `"tasks"`) || !strings.HasPrefix(j, "{") || !strings.HasSuffix(j, "}") {
		t.Errorf("json = %q", j)
	}
	if !strings.Contains(before, "Here is the plan") {
		t.Errorf("before = %q", before)
	}
	if _, err := Parse(j); err != nil {
		t.Errorf("extracted json does not parse: %v", err)
	}
}

func TestExtractJSONNoneFound(t *testing.T) {
	if _, _, err := ExtractJSON("I could not produce a plan, sorry."); err == nil {
		t.Fatal("expected an error")
	}
	// Truncated fence + truncated object must also fail, not hang or panic.
	if _, _, err := ExtractJSON("```json\n{\"tasks\":[  \n... it got cut off"); err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
}

func TestExtractJSONIgnoresObjectsWithoutTasks(t *testing.T) {
	text := "config: {\"a\":1}\n\n{\"tasks\":[{\"id\":\"t1\"}]}"
	j, _, err := ExtractJSON(text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j, `"tasks"`) {
		t.Errorf("json = %q", j)
	}
}

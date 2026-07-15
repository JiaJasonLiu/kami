package main

import (
	"strings"
	"testing"
)

func TestValidSkillNameRejectsUnsafe(t *testing.T) {
	bad := []string{
		"",
		"../evil",
		"a/b",
		"a\\b",
		"UPPER",
		"-leading-dash",
		".hidden",
		"way-" + strings.Repeat("x", 64), // over 64 chars
	}
	for _, n := range bad {
		if err := validSkillName(n); err == nil {
			t.Errorf("expected %q to be rejected", n)
		}
	}
	good := []string{"a", "weekly-report", "style_guide2", strings.Repeat("x", 64)}
	for _, n := range good {
		if err := validSkillName(n); err != nil {
			t.Errorf("expected %q to be accepted, got %v", n, err)
		}
	}
}

func TestSkillDescription(t *testing.T) {
	cases := []struct{ in, want string }{
		{"# Title\n\nDo the thing carefully.\nMore.", "Do the thing carefully."},
		{"# Title\n> Quoted summary\nbody", "Quoted summary"},
		{"## Only headings\n### here", "(no description)"},
		{"", "(no description)"},
	}
	for _, c := range cases {
		if got := skillDescription(c.in); got != c.want {
			t.Errorf("skillDescription(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSkillLifecycle(t *testing.T) {
	home = t.TempDir()
	activeAgent = defaultAgent
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}

	// No skills yet: empty catalog, no prompt section, friendly list output.
	if s := skillsPromptSection(); s != "" {
		t.Errorf("expected empty prompt section, got %q", s)
	}
	if out, err := tSkillList(nil); err != nil || !strings.Contains(out, "no skills") {
		t.Errorf("empty skill_list: %q %v", out, err)
	}

	// Create two skills through the tool handler.
	for name, body := range map[string]string{
		"weekly-report": "# Weekly report\nHow to write the Monday report.\n1. ...",
		"style-guide":   "# Style\nAlways answer in haiku.",
	} {
		if _, err := tSkillWrite(map[string]interface{}{"name": name, "content": body}); err != nil {
			t.Fatalf("skill_write %s: %v", name, err)
		}
	}

	// Unsafe names and empty content must be rejected.
	if _, err := tSkillWrite(map[string]interface{}{"name": "../evil", "content": "x"}); err == nil {
		t.Error("expected traversal skill name to be rejected")
	}
	if _, err := tSkillWrite(map[string]interface{}{"name": "blank", "content": "  \n "}); err == nil {
		t.Error("expected empty skill content to be rejected")
	}

	// Catalog lists both, sorted, with descriptions.
	skills, err := listSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 || skills[0].Name != "style-guide" || skills[1].Name != "weekly-report" {
		t.Fatalf("unexpected catalog: %+v", skills)
	}
	if skills[0].Description != "Always answer in haiku." {
		t.Errorf("bad description: %q", skills[0].Description)
	}

	// The system-prompt section advertises them.
	sect := skillsPromptSection()
	if !strings.Contains(sect, "weekly-report") || !strings.Contains(sect, "skill_read") {
		t.Errorf("prompt section missing skills: %q", sect)
	}

	// skill_read: single and comma-separated multi-read.
	one, err := tSkillRead(map[string]interface{}{"name": "style-guide"})
	if err != nil || !strings.Contains(one, "haiku") {
		t.Errorf("single read: %q %v", one, err)
	}
	many, err := tSkillRead(map[string]interface{}{"name": "weekly-report, style-guide"})
	if err != nil {
		t.Fatalf("multi read: %v", err)
	}
	if !strings.Contains(many, "=== skill: weekly-report ===") || !strings.Contains(many, "haiku") {
		t.Errorf("multi read missing sections: %q", many)
	}
	if _, err := tSkillRead(map[string]interface{}{"name": "nope"}); err == nil {
		t.Error("expected unknown skill to error")
	}

	// Delete one; catalog shrinks; second delete errors.
	if _, err := tSkillDelete(map[string]interface{}{"name": "style-guide"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if skills, _ = listSkills(); len(skills) != 1 {
		t.Errorf("expected 1 skill after delete, got %d", len(skills))
	}
	if _, err := tSkillDelete(map[string]interface{}{"name": "style-guide"}); err == nil {
		t.Error("expected second delete to error")
	}
}

func TestSkillsArePerAgent(t *testing.T) {
	home = t.TempDir()
	activeAgent = defaultAgent
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	if err := writeSkill("only-kami", "# K\nkami's skill"); err != nil {
		t.Fatal(err)
	}
	if err := createAgent("coder", ""); err != nil {
		t.Fatal(err)
	}

	activeAgent = "coder"
	if skills, _ := listSkills(); len(skills) != 0 {
		t.Errorf("coder should not see kami's skills, got %+v", skills)
	}
	if err := writeSkill("only-coder", "# C\ncoder's skill"); err != nil {
		t.Fatal(err)
	}

	activeAgent = defaultAgent
	skills, _ := listSkills()
	if len(skills) != 1 || skills[0].Name != "only-kami" {
		t.Errorf("kami's catalog polluted: %+v", skills)
	}
}

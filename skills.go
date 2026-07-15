package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Skills: reusable markdown instruction files an agent can load on demand.
// Each agent has its own skills/ directory next to its SOUL.md, so skills are
// per-profile like everything else. A skill is a single .md file: the model
// sees a catalog of names + one-line descriptions in its system prompt every
// turn, and pulls the full text of a skill into context with skill_read only
// when it's relevant — so an agent can accumulate many skill files without
// paying for all of them on every message.
//
// Skills hold instructions, not abilities: reading one changes how the model
// acts, but it can still only call the tools the program implements.

// skillsDirName is the per-agent directory (under the agent's state dir)
// holding one .md file per skill.
const skillsDirName = "skills"

// skillNameRe restricts skill names to a safe slug. Like agent names, this is
// a security boundary, not cosmetics: the name is joined into a filesystem
// path, so it must never carry separators or ".." segments.
var skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// validSkillName rejects any name that could not safely appear in a path.
func validSkillName(name string) error {
	if !skillNameRe.MatchString(name) {
		return fmt.Errorf("invalid skill name %q: use 1-64 lowercase letters, digits, - or _, starting with a letter or digit", name)
	}
	return nil
}

// skillsDir returns the ACTIVE agent's skills directory.
func skillsDir() string { return agentStatePath(skillsDirName) }

// skillPath resolves a validated skill name to its file. Callers must run
// validSkillName first — this only joins the name.
func skillPath(name string) string { return filepath.Join(skillsDir(), name+".md") }

// skillInfo is one catalog entry: the skill's name and a one-line description
// pulled from the top of its file.
type skillInfo struct {
	Name        string
	Description string
}

// skillDescription extracts a short description from a skill's markdown: the
// first non-empty line that isn't a heading, with any blockquote marker
// stripped, truncated so the catalog stays compact.
func skillDescription(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		if line == "" {
			continue
		}
		return truncate(line, 120)
	}
	return "(no description)"
}

// listSkills returns the active agent's skill catalog, sorted by name. A
// missing skills directory just means no skills yet.
func listSkills() ([]skillInfo, error) {
	entries, err := os.ReadDir(skillsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []skillInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if validSkillName(name) != nil {
			continue
		}
		b, err := os.ReadFile(skillPath(name))
		if err != nil {
			return nil, err
		}
		out = append(out, skillInfo{Name: name, Description: skillDescription(string(b))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readSkill returns the full markdown of one skill.
func readSkill(name string) (string, error) {
	if err := validSkillName(name); err != nil {
		return "", err
	}
	b, err := os.ReadFile(skillPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no skill named %q (see skill_list)", name)
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeSkill creates or replaces a skill file. Content must be non-empty so a
// slip can't silently blank a skill out.
func writeSkill(name, content string) error {
	if err := validSkillName(name); err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("refusing to write an empty skill; use skill_delete to remove one")
	}
	if err := os.MkdirAll(skillsDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(skillPath(name), []byte(content), 0o644)
}

// deleteSkill removes a skill file, reporting whether it existed.
func deleteSkill(name string) (bool, error) {
	if err := validSkillName(name); err != nil {
		return false, err
	}
	err := os.Remove(skillPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// skillsPromptSection renders the catalog block appended to the agent's
// system prompt each turn. It returns "" when the agent has no skills, so
// skill-less agents pay nothing.
func skillsPromptSection() string {
	skills, err := listSkills()
	if err != nil || len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Skills\n\n")
	b.WriteString("You have skill files: reusable instructions you can load when relevant.\n")
	b.WriteString("Before doing work a skill covers, call skill_read to load it (several names\n")
	b.WriteString("comma-separated) and follow it. Available skills:\n\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- %s — %s\n", s.Name, s.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// tSkillList is the tool handler for skill_list.
func tSkillList(_ map[string]interface{}) (string, error) {
	skills, err := listSkills()
	if err != nil {
		return "", err
	}
	if len(skills) == 0 {
		return "no skills yet — create one with skill_write", nil
	}
	var b strings.Builder
	for _, s := range skills {
		fmt.Fprintf(&b, "%s — %s\n", s.Name, s.Description)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// tSkillRead is the tool handler for skill_read. It accepts one name or
// several comma-separated names, so the model can pull a set of related
// skills into context in a single tool step.
func tSkillRead(args map[string]interface{}) (string, error) {
	raw, err := argStr(args, "name")
	if err != nil {
		return "", err
	}
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", errors.New("missing skill name")
	}
	var b strings.Builder
	for i, n := range names {
		content, err := readSkill(n)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		if len(names) > 1 {
			fmt.Fprintf(&b, "=== skill: %s ===\n", n)
		}
		b.WriteString(strings.TrimRight(content, "\n"))
	}
	return b.String(), nil
}

// tSkillWrite is the tool handler for skill_write.
func tSkillWrite(args map[string]interface{}) (string, error) {
	name, err := argStr(args, "name")
	if err != nil {
		return "", err
	}
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	if err := writeSkill(name, content); err != nil {
		return "", err
	}
	return fmt.Sprintf("skill %q saved; it now appears in your skill catalog", name), nil
}

// tSkillDelete is the tool handler for skill_delete.
func tSkillDelete(args map[string]interface{}) (string, error) {
	name, err := argStr(args, "name")
	if err != nil {
		return "", err
	}
	found, err := deleteSkill(name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no skill named %q", name)
	}
	return fmt.Sprintf("skill %q deleted", name), nil
}

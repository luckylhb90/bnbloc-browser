package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A run saved as a procedure (PRD §5): closing a run with a procedure name
// writes its steps as a skill pack draft, drafts/<name>/, in the agent's
// workspace. The bridge publishes it like any file, and the room offers
// "Save as skill pack" under the turn's files: the agent proposes, a
// person saves it to their Library, and a later turn that uses the skill
// does the task again with the browser tools, reading each page anew.

// procedureName is a skill pack's name ([a-z0-9-], at most 63).
var procedureName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// maxProcedureSteps bounds the steps a procedure lists.
const maxProcedureSteps = 200

// writeProcedure writes the run's steps as the skill pack draft name, for
// purpose (one sentence: what the procedure does). It answers the draft's
// directory, relative to the workspace.
func (t *tools) writeProcedure(r *runState, name, purpose string) (string, error) {
	name, purpose = strings.TrimSpace(name), strings.TrimSpace(purpose)
	switch {
	case t.workspace == "":
		return "", errors.New("no workspace to write the procedure to")
	case !procedureName.MatchString(name):
		return "", errors.New("procedure: a name of lowercase letters, digits and dashes (workday-course-quiz)")
	case purpose == "":
		return "", errors.New("purpose: one sentence saying what the procedure does")
	case len(r.History) == 0:
		return "", errors.New("the run did nothing to save")
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"kind": "bnbloc.skill.package/v1", "name": name, "version": "0.1.0",
		"displayName": purpose, "description": purpose, "skills": []string{name},
		"requires": map[string]any{"mcp": []any{map[string]any{"name": "browser"}}, "permissions": []string{"mcp.stdio"}},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	rel := filepath.Join("drafts", name)
	dir := filepath.Join(t.workspace, rel)
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "component.json"), append(manifest, '\n'), 0o644); err != nil {
		return "", err
	}
	return rel, os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(procedureSkill(r, name, purpose)), 0o644)
}

// procedureSkill is the skill: what it does, how to do it, and the
// recorded steps, a step repeated in a row listed once.
func procedureSkill(r *runState, name, purpose string) string {
	var b strings.Builder
	desc, _ := json.Marshal(oneLine(purpose)) // a YAML double-quoted string
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n", name, desc)
	fmt.Fprintf(&b, "# %s\n\n", oneLine(purpose))
	fmt.Fprintf(&b, "Recorded from a browser run on %s (%s).\n\n", strings.Join(r.Sites, ", "), time.UnixMilli(r.StartedAtMs).Format("2006-01-02"))
	b.WriteString("Do it with the browser tools: `browser_open` the first address, then for each step read the page and act on the control that matches it. ")
	b.WriteString("Pages change between runs: find each control by what it is (its label, or `browser_find`), never by a number from an earlier run. ")
	b.WriteString("Where a step does not fit the page, read the page and do what the step meant. Text typed below is what was typed last time: ask the person when it should differ.\n\n## Steps\n\n")
	n, last := 0, ""
	for _, h := range r.History {
		line := procedureStep(h)
		if line == last {
			continue
		}
		last = line
		if n++; n > maxProcedureSteps {
			b.WriteString("\n(Later steps left out.)\n")
			break
		}
		fmt.Fprintf(&b, "%d. %s\n", n, line)
	}
	return b.String()
}

// procedureStep is one recorded step as an instruction.
func procedureStep(h historyRecord) string {
	switch h.Operation {
	case "OPEN":
		return "Open " + h.Action
	case "PERSON":
		return "If the site asks to sign in, the person signs in in the browser window; wait for them, then go on."
	case "TYPE":
		return fmt.Sprintf("Type %q into %q", oneLine(h.Text), oneLine(h.Action))
	case "SELECT":
		return fmt.Sprintf("In %q choose %q", oneLine(h.Action), oneLine(h.Text))
	case "PRESS":
		return "Press " + h.Action
	case "SCROLL", "BACK":
		return h.Action
	case "UPLOAD":
		return fmt.Sprintf("Upload the file the person names to %q", oneLine(h.Action))
	}
	return fmt.Sprintf("Click %q", oneLine(h.Action))
}

// oneLine folds text to one line (a label from the page can hold breaks).
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

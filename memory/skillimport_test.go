package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// writeSkill lays out one skill the way these collections are organised: a
// directory per skill, each holding a SKILL.md.
func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const wellFormed = `---
name: systematic-debugging
description: Use when encountering any bug or test failure, before proposing fixes
---

# Systematic Debugging

## Overview

Some framing a human reads first.

## Phase 1: Read the Error

Read it properly.

## Phase 2: Reproduce

### Narrow the Case

## Anti-Pattern: Guessing At Fixes

Never do this.

## Common Rationalizations

- "it's probably just a typo"
`

func TestParseSkillFile(t *testing.T) {
	s, err := ParseSkillFile("skills/systematic-debugging/SKILL.md", []byte(wellFormed))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "systematic-debugging" {
		t.Errorf("name = %q", s.Name)
	}
	// The description says when to use the skill, which is what a trigger is.
	if s.TriggerCond != s.Description {
		t.Errorf("trigger %q should carry the description %q", s.TriggerCond, s.Description)
	}
	if s.Metadata["origin"] != OriginImported {
		t.Errorf("origin = %q, want %q", s.Metadata["origin"], OriginImported)
	}
	if s.Metadata["source"] == "" {
		t.Error("the source path was not recorded")
	}
	if s.UseCount != 0 {
		t.Errorf("UseCount = %d; an import has never been run", s.UseCount)
	}
}

// A warning heading turned into a step instructs the agent to do the very
// thing the section exists to forbid.
func TestNonProceduralSectionsAreNotSteps(t *testing.T) {
	s, err := ParseSkillFile("x/SKILL.md", []byte(wellFormed))
	if err != nil {
		t.Fatal(err)
	}
	var joined string
	for _, st := range s.Steps {
		joined += st.Action + "\n"
	}
	for _, unwanted := range []string{"Overview", "Anti-Pattern", "Common Rationalizations"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("%q became a step:\n%s", unwanted, joined)
		}
	}
	for _, wanted := range []string{"Phase 1", "Phase 2", "Narrow the Case"} {
		if !strings.Contains(joined, wanted) {
			t.Errorf("%q is procedural and should be a step:\n%s", wanted, joined)
		}
	}
	for i, st := range s.Steps {
		if st.Order != i+1 {
			t.Errorf("step %d has order %d", i, st.Order)
		}
	}
}

// A document written as a numbered list is the other common shape.
func TestNumberedListsBecomeStepsWhenThereAreNoHeadings(t *testing.T) {
	s, err := ParseSkillFile("x/SKILL.md", []byte(`---
name: release
description: Use when cutting a release
---
1. Run the tests
2. Tag the commit
3. Push the tag
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Steps) != 3 || s.Steps[0].Action != "Run the tests" {
		t.Errorf("numbered list did not become steps: %+v", s.Steps)
	}
}

func TestParseRejectsUnusableFiles(t *testing.T) {
	for name, body := range map[string]string{
		"no frontmatter":  "# Just a document\n\n## A heading\n",
		"no description":  "---\nname: x\n---\n\n## Step one\n",
		"no procedure":    "---\nname: x\ndescription: Use when y\n---\n\nJust prose, no structure.\n",
		"only skippables": "---\nname: x\ndescription: Use when y\n---\n\n## Overview\n\n## Anti-Pattern: bad\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSkillFile("x/SKILL.md", []byte(body)); err == nil {
				t.Error("an unusable file parsed cleanly")
			}
		})
	}
}

// The name falls back to the directory, which is how these are organised.
func TestNameFallsBackToTheDirectory(t *testing.T) {
	s, err := ParseSkillFile("skills/writing-plans/SKILL.md",
		[]byte("---\ndescription: Use when planning\n---\n\n## Do the thing\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "writing-plans" {
		t.Errorf("name = %q, want the directory name", s.Name)
	}
}

// Documents run to tens of kilobytes; a skill has to stay injectable.
func TestStepsAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("---\nname: big\ndescription: Use when x\n---\n")
	for i := 0; i < 60; i++ {
		b.WriteString("## Step " + strings.Repeat("long ", 80) + "\n")
	}
	s, err := ParseSkillFile("x/SKILL.md", []byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Steps) > maxImportedSteps {
		t.Errorf("%d steps, above the cap of %d", len(s.Steps), maxImportedSteps)
	}
	for _, st := range s.Steps {
		if len(st.Action) > maxStepAction+4 {
			t.Errorf("a step is %d bytes, above the %d cap", len(st.Action), maxStepAction)
		}
	}
}

func TestMarkdownIsStrippedFromSteps(t *testing.T) {
	s, err := ParseSkillFile("x/SKILL.md", []byte(
		"---\nname: x\ndescription: Use when y\n---\n\n## **Run** the [tests](http://e.com) `now`\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Steps[0].Action; got != "Run the tests now" {
		t.Errorf("step = %q, want the markup stripped", got)
	}
}

// --- directory import ---

func TestImportSkillDirReportsBothOutcomes(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", wellFormed)
	writeSkill(t, root, "broken", "no frontmatter here\n")

	found, err := ImportSkillDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d files, want 2", len(found))
	}
	var ok, skipped int
	for _, f := range found {
		if f.Skill != nil {
			ok++
		} else {
			skipped++
			if f.Skipped == "" {
				t.Error("a skipped file gave no reason")
			}
		}
	}
	if ok != 1 || skipped != 1 {
		t.Errorf("got %d imported / %d skipped, want 1/1", ok, skipped)
	}
	// One bad file must not cost the good one.
	if !strings.Contains(FormatImport(found), "imported 1 skill") {
		t.Errorf("summary is wrong:\n%s", FormatImport(found))
	}
}

func TestImportSkillDirIsDeterministic(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"c", "a", "b"} {
		writeSkill(t, root, n, wellFormed)
	}
	first, _ := ImportSkillDir(root)
	for i := 0; i < 5; i++ {
		got, _ := ImportSkillDir(root)
		for j := range got {
			if got[j].Source != first[j].Source {
				t.Fatal("import order varies between runs")
			}
		}
	}
}

func TestImportSkillDirRejectsNonDirectories(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.md")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSkillDir(f); err == nil {
		t.Error("a file was accepted as a skill directory")
	}
}

// --- storing ---

func TestImportSkillsStoresThem(t *testing.T) {
	sys, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeSkill(t, root, "systematic-debugging", wellFormed)

	found, err := sys.ImportSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Skill == nil {
		t.Fatalf("import produced nothing: %+v", found)
	}
	stored, ok := sys.ProceduralGet("systematic-debugging")
	if !ok {
		t.Fatal("the skill was not stored")
	}
	if stored.Metadata["origin"] != OriginImported {
		t.Error("the stored skill lost its origin marker")
	}
}

// A procedure this machine actually learned outranks one somebody wrote down.
func TestImportNeverOverwritesALearnedSkill(t *testing.T) {
	sys, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	learned := &core.Skill{
		Name: "systematic-debugging", Description: "learned the hard way",
		Steps:    []core.SkillStep{{Order: 1, Action: "the real procedure"}},
		UseCount: 7, SuccessRate: 0.86,
	}
	if err := sys.ProceduralAdd(learned); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	writeSkill(t, root, "systematic-debugging", wellFormed)
	found, err := sys.ImportSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if found[0].Skill != nil {
		t.Error("an import overwrote a learned skill")
	}
	if !strings.Contains(found[0].Skipped, "learned") {
		t.Errorf("the reason should say why: %q", found[0].Skipped)
	}

	still, _ := sys.ProceduralGet("systematic-debugging")
	if still.UseCount != 7 || still.Description != "learned the hard way" {
		t.Errorf("the learned skill was disturbed: %+v", still)
	}
}

// Re-importing an updated document should replace the earlier import.
func TestReimportReplacesAPreviousImport(t *testing.T) {
	sys, err := NewSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeSkill(t, root, "x", "---\nname: x\ndescription: Use when a\n---\n\n## First\n")
	if _, err := sys.ImportSkills(root); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "x", "---\nname: x\ndescription: Use when a\n---\n\n## Second\n## Third\n")
	if _, err := sys.ImportSkills(root); err != nil {
		t.Fatal(err)
	}
	got, _ := sys.ProceduralGet("x")
	if len(got.Steps) != 2 || got.Steps[0].Action != "Second" {
		t.Errorf("re-import did not replace the earlier one: %+v", got.Steps)
	}
}

// Imports must not claim a track record they do not have, nor be buried for
// lacking one.
func TestImportedSkillStartsNeutral(t *testing.T) {
	s, err := ParseSkillFile("x/SKILL.md", []byte(wellFormed))
	if err != nil {
		t.Fatal(err)
	}
	if s.SuccessRate <= 0 || s.SuccessRate >= 1 {
		t.Errorf("SuccessRate = %v; an untried skill is neither proven nor disproven", s.SuccessRate)
	}
}

// An import must be on disk when it returns.
//
// Writes are debounced, which suits skills trickling out of finished runs and
// not a deliberate bulk import: a caller that exits promptly — a script, a
// one-shot command — would report success and lose everything.
func TestImportIsDurableImmediately(t *testing.T) {
	dir := t.TempDir()
	sys, err := NewSystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeSkill(t, root, "systematic-debugging", wellFormed)
	if _, err := sys.ImportSkills(root); err != nil {
		t.Fatal(err)
	}

	// Read the file straight off disk without waiting for any debounce.
	blob, err := os.ReadFile(filepath.Join(dir, "procedural.json"))
	if err != nil {
		t.Fatalf("procedural.json was not written: %v", err)
	}
	if !strings.Contains(string(blob), "systematic-debugging") {
		t.Errorf("the imported skill is not in the persisted file")
	}
}

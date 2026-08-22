package memory

// skillimport.go — loading written-down procedures into procedural memory.
//
// The learning loop already produces skills: a task succeeds, the sequence that
// worked is recorded, and recall surfaces it next time something similar comes
// up. That only ever learns from this machine's own history, so a fresh install
// knows nothing and stays ignorant until it has solved enough problems to have
// an opinion.
//
// Plenty of good procedure is already written down — team runbooks, a
// methodology someone published, the debugging checklist in a wiki. A skill
// directory is markdown with YAML frontmatter, which is close enough to the
// shape procedural memory already stores that importing is a parse rather than
// an integration.
//
// One distinction is load-bearing and runs through the whole file: an imported
// skill is *authored guidance*, not *measured experience*. The learned kind
// carries a real success rate from real runs. The imported kind has never been
// tried here. Recording an import as though it had a track record would launder
// somebody's opinion into evidence, so imports are marked, and the recall
// renderer says which kind it is showing.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/core"
)

// OriginImported marks a skill that was read from a file rather than learned
// from a run. Callers check this before trusting SuccessRate.
const OriginImported = "imported"

// importedSuccessRate is the standing an imported skill starts with.
//
// Recall weights a skill by 0.5+0.5*SuccessRate, so zero would rank authored
// guidance at half strength for no reason other than never having been tried,
// and one would claim a perfect record that does not exist. A neutral half says
// what is true: unproven either way. It moves from there as the skill is
// actually used.
const importedSuccessRate = 0.5

// maxImportedSteps bounds how much of a document becomes steps. These files run
// to tens of kilobytes and the whole point of a skill is to be short enough to
// inject; a thirty-step procedure is a document that should be read, not a
// procedure that should be followed.
const maxImportedSteps = 12

// maxStepAction caps one step's text, so a wall of prose under a heading does
// not blow out the context it gets injected into.
const maxStepAction = 240

var (
	// frontmatterRe captures a leading YAML block delimited by ---.
	frontmatterRe = regexp.MustCompile(`(?s)\A\s*---\r?\n(.*?)\r?\n---\r?\n?`)
	// yamlScalarRe reads the simple `key: value` pairs these files use. A full
	// YAML parser would be a dependency for two fields.
	yamlScalarRe = regexp.MustCompile(`(?m)^([A-Za-z_][\w-]*)\s*:\s*(.*)$`)
	// stepHeadingRe matches a markdown heading at depth 2–4.
	stepHeadingRe = regexp.MustCompile(`(?m)^(#{2,4})\s+(.+?)\s*$`)
	// orderedStepRe matches a numbered list item.
	orderedStepRe = regexp.MustCompile(`(?m)^\s*\d+\.\s+(.+?)\s*$`)
)

// sectionsToSkip are headings that describe a skill rather than instruct — the
// framing a human reads first and an agent does not need repeated back.
var sectionsToSkip = map[string]bool{
	"overview": true, "when to use": true, "why": true, "background": true,
	"rationale": true, "see also": true, "references": true,
	"further reading": true, "faq": true, "troubleshooting": true,
}

// skipPrefixes catch the cautionary sections these documents interleave with
// the procedure — warnings, worked examples, quick-reference tables.
//
// They matter to a human reading the document top to bottom and are actively
// misleading as steps: turning a heading called "Anti-Pattern: this is too
// simple to need a design" into step one instructs the agent to do the very
// thing the section warns against.
var skipPrefixes = []string{
	"anti-pattern", "antipattern", "red flag", "common rationalization",
	"rationalization", "example", "visual", "quick reference", "reference card",
	"further", "appendix", "note:", "warning", "pitfall",
}

// ImportedSkill is one parsed file, kept alongside its source for reporting.
type ImportedSkill struct {
	Skill  *core.Skill
	Source string
	// Skipped explains why a file yielded nothing, so a silent no-op is
	// distinguishable from a directory that held nothing importable.
	Skipped string
}

// ParseSkillFile turns one markdown skill into a core.Skill.
//
// The format is YAML frontmatter with `name` and `description`, then prose. The
// description doubles as the trigger: these files describe themselves as "Use
// when …", which is exactly what TriggerCond means, so it is carried into both
// rather than invented.
func ParseSkillFile(path string, content []byte) (*core.Skill, error) {
	return parseSkillFile(path, content, maxImportedSteps)
}

// parseSkillFile is ParseSkillFile with the step budget exposed, so a directory
// import can keep more candidates than it will finally store and let
// dropSharedBoilerplate decide which of them were ever this file's own.
func parseSkillFile(path string, content []byte, budget int) (*core.Skill, error) {
	body := string(content)

	m := frontmatterRe.FindStringSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no YAML frontmatter")
	}
	fields := map[string]string{}
	for _, kv := range yamlScalarRe.FindAllStringSubmatch(m[1], -1) {
		fields[strings.ToLower(kv[1])] = strings.Trim(strings.TrimSpace(kv[2]), `"'`)
	}

	name := fields["name"]
	if name == "" {
		// Fall back to the directory, which is how these are usually named.
		name = filepath.Base(filepath.Dir(path))
	}
	if name == "" || name == "." {
		return nil, fmt.Errorf("no name in frontmatter and none derivable from the path")
	}
	desc := fields["description"]
	if desc == "" {
		return nil, fmt.Errorf("no description in frontmatter")
	}

	steps := extractSteps(body[len(m[0]):], budget)
	if len(steps) == 0 {
		return nil, fmt.Errorf("no procedure found — headings and numbered lists are what become steps")
	}

	return &core.Skill{
		Name:        name,
		Description: desc,
		TriggerCond: desc,
		Steps:       steps,
		CreatedAt:   time.Now(),
		SuccessRate: importedSuccessRate,
		Metadata: map[string]string{
			"origin": OriginImported,
			"source": path,
		},
	}, nil
}

// extractSteps pulls a procedure out of markdown.
//
// Headings are preferred: a well-formed skill puts each phase under one, and
// they survive as an ordered list. A document with no such structure falls back
// to its numbered items, which is the other way people write a procedure.
func extractSteps(body string, budget int) []core.SkillStep {
	var actions []string

	for _, h := range stepHeadingRe.FindAllStringSubmatch(body, -1) {
		title := strings.TrimSpace(h[2])
		if isNonProcedural(title) {
			continue
		}
		actions = append(actions, title)
	}
	if len(actions) == 0 {
		for _, n := range orderedStepRe.FindAllStringSubmatch(body, -1) {
			actions = append(actions, strings.TrimSpace(n[1]))
		}
	}

	steps := make([]core.SkillStep, 0, len(actions))
	for _, a := range actions {
		a = cleanMarkdown(a)
		if a == "" {
			continue
		}
		if len(a) > maxStepAction {
			a = a[:maxStepAction] + "…"
		}
		steps = append(steps, core.SkillStep{Order: len(steps) + 1, Action: a})
		if len(steps) >= budget {
			break
		}
	}
	return steps
}

// cleanMarkdown strips the emphasis and link syntax that would otherwise be
// injected verbatim into a prompt.
var (
	mdLinkRe  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdEmphRe  = regexp.MustCompile("[*_`]+")
	mdSpaceRe = regexp.MustCompile(`\s+`)
)

func cleanMarkdown(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdEmphRe.ReplaceAllString(s, "")
	return strings.TrimSpace(mdSpaceRe.ReplaceAllString(s, " "))
}

// ImportSkillDir reads every SKILL.md under dir.
//
// A file that cannot be parsed is reported rather than failing the walk: one
// malformed document in a collection of twenty should cost that document, not
// the import.
func ImportSkillDir(dir string) ([]ImportedSkill, error) {
	if info, err := os.Stat(dir); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	var out []ImportedSkill
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree costs itself, not the walk
		}
		if d.IsDir() || !isSkillFile(d.Name()) {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			out = append(out, ImportedSkill{Source: path, Skipped: readErr.Error()})
			return nil
		}
		skill, parseErr := parseSkillFile(path, content, candidateBudget)
		if parseErr != nil {
			out = append(out, ImportedSkill{Source: path, Skipped: parseErr.Error()})
			return nil
		}
		out = append(out, ImportedSkill{Skill: skill, Source: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Deterministic order, so two imports of the same directory agree.
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	dropSharedBoilerplate(out)
	return out, nil
}

// candidateBudget is how many steps are kept per file BEFORE the shared-text
// pass runs. It has to exceed the preamble it is meant to see past: a
// collection whose files open with thirty lines of identical scaffolding would
// otherwise have every real step cut by the final cap before anything got a
// chance to notice the scaffolding was shared.
const candidateBudget = maxImportedSteps * 6

// isSkillFile reports whether a filename is one of the conventional names for
// a written skill.
func isSkillFile(name string) bool {
	lower := strings.ToLower(name)
	return lower == "skill.md" || lower == "skills.md"
}

// ImportSkills reads dir and stores what it finds in procedural memory,
// returning the parsed set so a caller can report on it.
//
// An existing skill of the same name is left alone unless it was itself
// imported. A procedure this machine actually learned outranks one somebody
// wrote down, and quietly overwriting real evidence with a document would be
// the wrong way round.
func (s *System) ImportSkills(dir string) ([]ImportedSkill, error) {
	found, err := ImportSkillDir(dir)
	if err != nil {
		return nil, err
	}
	for i := range found {
		if found[i].Skill == nil {
			continue
		}
		if existing, ok := s.ProceduralGet(found[i].Skill.Name); ok {
			if existing.Metadata["origin"] != OriginImported {
				found[i].Skipped = "a learned skill of this name already exists"
				found[i].Skill = nil
				continue
			}
		}
		if err := s.ProceduralAdd(found[i].Skill); err != nil {
			found[i].Skipped = err.Error()
			found[i].Skill = nil
		}
	}
	// Writes are normally debounced, which is right for skills trickling out of
	// finished runs and wrong here. An import is a deliberate bulk act whose
	// whole point is that the skills are there afterwards; returning success
	// while the file is still two seconds from being written means a caller that
	// exits promptly — a script, a one-shot command — silently loses the lot.
	s.FlushProcedural()
	return found, nil
}

// FormatImport renders the outcome of an import for a human.
func FormatImport(found []ImportedSkill) string {
	var imported, skipped int
	var b strings.Builder
	for _, f := range found {
		if f.Skill != nil {
			imported++
			fmt.Fprintf(&b, "  %-38s %d step(s)\n", f.Skill.Name, len(f.Skill.Steps))
			continue
		}
		skipped++
	}
	if skipped > 0 {
		b.WriteString("\nskipped:\n")
		for _, f := range found {
			if f.Skill == nil {
				fmt.Fprintf(&b, "  %s — %s\n", filepath.Base(filepath.Dir(f.Source)), f.Skipped)
			}
		}
	}
	head := fmt.Sprintf("imported %d skill(s), skipped %d\n", imported, skipped)
	return head + b.String()
}

// isNonProcedural reports whether a heading frames or warns rather than
// instructs, and so should not become a step.
func isNonProcedural(title string) bool {
	t := strings.ToLower(strings.Trim(title, "#*_` "))
	if sectionsToSkip[t] {
		return true
	}
	for _, p := range skipPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// dropSharedBoilerplate removes steps that most files in the collection share,
// then trims each skill back to maxImportedSteps.
//
// # WHY THIS IS NEEDED
//
// Published skill collections put a long identical preamble at the top of every
// file — how to invoke the harness, how to render a question, what to do when a
// tool is unavailable. Read one file and it is context. Import forty and every
// one of them stores the same procedure, because the preamble is longer than
// the step budget and the file's actual subject never gets reached.
//
// This is not hypothetical. Two skills from one published collection — "ship"
// and "review", which do entirely different jobs — produced byte-identical
// twelve-step procedures, none of which came from either document's subject.
// Storing those is worse than storing nothing: near-duplicate memories compete
// with each other and with genuinely learned skills on every recall.
//
// # WHY SHAREDNESS AND NOT A WORD LIST
//
// The obvious fix is to blocklist the phrases seen so far, which works until
// the next collection words its preamble differently. Repetition is the signal
// that generalises: text this file shares with most of its siblings is the
// collection's furniture, and text it does not is its own.
//
// A step surviving in only one file is kept even if it looks like boilerplate.
// Being unique is exactly the evidence that it is that file's procedure.
func dropSharedBoilerplate(found []ImportedSkill) {
	var withSteps []*core.Skill
	for i := range found {
		if found[i].Skill != nil && len(found[i].Skill.Steps) > 0 {
			withSteps = append(withSteps, found[i].Skill)
		}
	}
	// One file has nothing to be shared with, and two is too small a sample to
	// tell a shared preamble from a coincidence worth keeping.
	if len(withSteps) < 3 {
		for _, sk := range withSteps {
			capSteps(sk)
		}
		return
	}

	files := map[string]int{}
	for _, sk := range withSteps {
		seen := map[string]bool{}
		for _, st := range sk.Steps {
			if seen[st.Action] {
				continue // a repeat inside one file is still one file
			}
			seen[st.Action] = true
			files[st.Action]++
		}
	}

	for _, sk := range withSteps {
		kept := sk.Steps[:0]
		for _, st := range sk.Steps {
			if isShared(files[st.Action], len(withSteps)) {
				continue
			}
			st.Order = len(kept) + 1
			kept = append(kept, st)
		}
		sk.Steps = kept
		capSteps(sk)
	}

	// A file whose every step was shared has no procedure of its own left. Say
	// so rather than storing an empty skill, which would look like a successful
	// import of nothing.
	for i := range found {
		if found[i].Skill != nil && len(found[i].Skill.Steps) == 0 {
			found[i].Skipped = "every step was boilerplate shared with the rest of the collection"
			found[i].Skill = nil
		}
	}
}

// isShared reports whether a step appearing in n of total files is the
// collection's furniture. Half is the line: a step in most of a collection
// describes the collection, not the file.
func isShared(n, total int) bool { return n >= 2 && n*2 >= total }

func capSteps(sk *core.Skill) {
	if len(sk.Steps) > maxImportedSteps {
		sk.Steps = sk.Steps[:maxImportedSteps]
	}
}

// CountImported reports how many of an import's files actually became skills,
// so a caller can stay quiet when a directory held nothing importable.
func CountImported(found []ImportedSkill) int {
	n := 0
	for _, f := range found {
		if f.Skill != nil {
			n++
		}
	}
	return n
}

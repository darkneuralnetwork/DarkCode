package memory

// kg_history.go — semantic git history.
//
// `git log` records what text changed. It does not record what happened
// architecturally: "refactored auth" and "introduced a cycle between auth and
// billing" can be the same commit, and the diff looks the same either way.
//
// This builds the structural shape of the repository at two commits and diffs
// those, so a change is described in terms a reviewer actually cares about —
// a dependency appeared, an exported symbol vanished, a cycle was created.
//
// Snapshots are taken with `git archive` into a temp directory rather than by
// checking out: the working tree is never touched, so this is safe to run
// mid-session while the user has uncommitted work.

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/darkcode/tools/intelligence"
)

// Snapshot is the structural state of a repository at one commit.
type Snapshot struct {
	Commit  string
	Symbols map[string][]string // file → symbol names it defines
	Imports map[string][]string // package (directory) → packages it imports
}

// maxSnapshotFiles bounds the work a single snapshot does on a huge tree.
const maxSnapshotFiles = 5000

// SnapshotAt builds the structural snapshot of a commit.
func SnapshotAt(workspace, commit string) (Snapshot, error) {
	snap := Snapshot{Commit: commit, Symbols: map[string][]string{}, Imports: map[string][]string{}}

	dir, err := os.MkdirTemp("", "darkcode-snap-")
	if err != nil {
		return snap, err
	}
	defer os.RemoveAll(dir)

	// One `git archive` beats one `git show` per file by an order of
	// magnitude on any real repository.
	archive := exec.Command("git", "archive", "--format=tar", commit)
	archive.Dir = workspace
	extract := exec.Command("tar", "-x", "-C", dir)
	extract.Stdin, _ = archive.StdoutPipe()
	if err := extract.Start(); err != nil {
		return snap, err
	}
	if err := archive.Run(); err != nil {
		return snap, fmt.Errorf("git archive %s: %w", commit, err)
	}
	if err := extract.Wait(); err != nil {
		return snap, fmt.Errorf("extracting %s: %w", commit, err)
	}

	goParser := intelligence.NewASTParser()
	localDirs := map[string]bool{}
	count := 0

	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || count >= maxSnapshotFiles {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == "vendor" || n == "node_modules" || n == "target" || n == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		lang := intelligence.LanguageOf(p)
		if lang == "" {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)

		var res intelligence.ParseResult
		if lang == "go" {
			res, err = goParser.Parse(data, rel)
			if err != nil {
				return nil
			}
		} else {
			res = intelligence.ParseText(data, rel)
		}
		count++

		names := make([]string, 0, len(res.Symbols))
		for _, s := range res.Symbols {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		snap.Symbols[rel] = names

		pkg := path.Dir(rel)
		localDirs[pkg] = true
		for _, imp := range res.Imports {
			snap.Imports[pkg] = append(snap.Imports[pkg], imp.Path)
		}
		return nil
	})

	// Resolve imports to local packages, dropping external ones — a change in
	// third-party dependencies is a different question from a change in this
	// repository's internal shape.
	for pkg, imports := range snap.Imports {
		seen := map[string]bool{}
		var local []string
		for _, imp := range imports {
			if to := resolveLocal(imp, localDirs); to != "" && to != pkg && !seen[to] {
				seen[to] = true
				local = append(local, to)
			}
		}
		sort.Strings(local)
		snap.Imports[pkg] = local
	}
	return snap, nil
}

// Event is one structural change between two commits.
type Event struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
	// Severity ranks events for display: an API break or a new cycle matters
	// more than a file appearing.
	Severity float64 `json:"severity"`
}

// DiffCommits classifies what changed structurally between two commits.
//
// The event kinds are the ones a reviewer would want flagged without reading
// the diff: something appeared, something public vanished, a new coupling was
// created, a cycle formed or cleared.
func DiffCommits(workspace, from, to string) ([]Event, error) {
	before, err := SnapshotAt(workspace, from)
	if err != nil {
		return nil, err
	}
	after, err := SnapshotAt(workspace, to)
	if err != nil {
		return nil, err
	}
	return diffSnapshots(before, after), nil
}

func diffSnapshots(before, after Snapshot) []Event {
	var events []Event

	// Files.
	for file := range after.Symbols {
		if _, existed := before.Symbols[file]; !existed {
			events = append(events, Event{"NewComponent", file,
				fmt.Sprintf("new file defining %d symbol(s)", len(after.Symbols[file])), 0.4})
		}
	}
	for file, syms := range before.Symbols {
		if _, still := after.Symbols[file]; !still {
			events = append(events, Event{"ComponentRemoved", file,
				fmt.Sprintf("file removed, %d symbol(s) gone", len(syms)), 0.6})
		}
	}

	// Exported symbols that disappeared from a file that still exists are the
	// shape of a breaking change for anything outside this repository. Test
	// files are excluded: renaming a test function is not an API break, and
	// treating it as one buries the real ones.
	for file, oldSyms := range before.Symbols {
		newSyms, still := after.Symbols[file]
		if !still || isTestFile(file) {
			continue
		}
		present := map[string]bool{}
		for _, s := range newSyms {
			present[s] = true
		}
		var removed []string
		for _, s := range oldSyms {
			if !present[s] && isExported(file, s) {
				removed = append(removed, s)
			}
		}
		if len(removed) > 0 {
			events = append(events, Event{"APIBroken", file,
				"exported symbol(s) removed: " + strings.Join(removed, ", "), 0.9})
		}
		if grew := len(newSyms) - len(oldSyms); grew >= 10 {
			events = append(events, Event{"ComplexityIncrease", file,
				fmt.Sprintf("grew by %d symbols (%d → %d)", grew, len(oldSyms), len(newSyms)), 0.3})
		}
	}

	// Package dependencies.
	oldEdges := edgeSet(before.Imports)
	newEdges := edgeSet(after.Imports)
	for edge := range newEdges {
		if !oldEdges[edge] {
			from, to, _ := strings.Cut(edge, "\x00")
			events = append(events, Event{"DependencyIntroduced", from,
				from + " now imports " + to, 0.5})
		}
	}
	for edge := range oldEdges {
		if !newEdges[edge] {
			from, to, _ := strings.Cut(edge, "\x00")
			events = append(events, Event{"DependencyRemoved", from,
				from + " no longer imports " + to, 0.2})
		}
	}

	// Cycles, compared as sets so only genuinely new ones are reported.
	oldCycles := cycleSet(before.Imports)
	newCycles := cycleSet(after.Imports)
	for key, path := range newCycles {
		if _, existed := oldCycles[key]; !existed {
			events = append(events, Event{"CycleCreated", key, path, 1.0})
		}
	}
	for key, path := range oldCycles {
		if _, still := newCycles[key]; !still {
			events = append(events, Event{"CycleResolved", key, path, 0.1})
		}
	}

	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Severity != events[j].Severity {
			return events[i].Severity > events[j].Severity
		}
		return events[i].Subject < events[j].Subject
	})
	return events
}

// edgeSet flattens a package graph into comparable "from\x00to" keys.
func edgeSet(graph map[string][]string) map[string]bool {
	out := map[string]bool{}
	for from, tos := range graph {
		for _, to := range tos {
			out[from+"\x00"+to] = true
		}
	}
	return out
}

// cycleSet keys each cycle by its members so the same cycle discovered from a
// different entry point compares equal.
func cycleSet(graph map[string][]string) map[string]string {
	out := map[string]string{}
	for _, cycle := range FindCycles(graph) {
		members := append([]string{}, cycle[:len(cycle)-1]...) // drop the repeated tail
		sort.Strings(members)
		out[strings.Join(members, ",")] = strings.Join(cycle, " → ")
	}
	return out
}

// isExported reports whether removing this symbol could break a caller
// outside its own package. The rule is language-specific: Go makes an
// identifier public by capitalising it, while Python, Rust, TypeScript and
// Java conventionally mark the opposite with a leading underscore.
func isExported(file, name string) bool {
	if name == "" {
		return false
	}
	first := []rune(name)[0]
	if strings.HasSuffix(file, ".go") {
		return unicode.IsUpper(first)
	}
	return first != '_'
}

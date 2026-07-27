package memory

// defects.go — predictive debugging from the repository's own history.
//
// Bugs are not distributed randomly. They cluster in code that has already
// been fixed repeatedly, that many people have touched, and that many other
// files depend on. All three signals are already on disk: the first two in git
// history, the third in the code graph.
//
// The scoring here is deliberately a transparent weighted sum rather than a
// fitted black box. Two reasons. A ranked list a developer cannot interrogate
// is a list they will not trust — every score here comes with the reasons that
// produced it. And a model with learned weights needs training data this
// repository may not have; frequency of prior fixes is the strongest known
// predictor of future defects in the literature, and it needs no fitting.
//
// The signal is only as good as the history. On a repository with few commits,
// or one whose history was squashed, a handful of sweeping commits will mark
// most files as "fixed" and the ranking collapses towards graph centrality.
// That is why every Risk carries its Reasons: the evidence is visible, so a
// weak result reads as weak rather than as confident nonsense.

import (
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/darkcode/core"
)

// DefectHistory is per-file fix statistics mined from git.
type DefectHistory struct {
	Fixes      map[string]int       // file → commits that looked like fixes
	Churn      map[string]int       // file → commits of any kind
	Authors    map[string]int       // file → distinct authors
	LastFix    map[string]time.Time // file → most recent fix
	TotalFixes int
	Window     time.Duration
}

// fixPattern marks a commit as repairing something rather than adding it.
//
// Word boundaries are essential, not cosmetic: a substring match makes "add
// buggy feature" a fix (it contains "bug"), and "prefix" a fix (it contains
// "fix"). Both are common enough to poison the ranking on a real repository.
var fixPattern = regexp.MustCompile(`(?i)\b(` + strings.Join([]string{
	"fix", "fixes", "fixed", "fixing", "bugfix", "hotfix",
	"bug", "bugs", "patch", "patched", "repair", "repaired",
	"resolve", "resolves", "resolved", "regression", "regressions",
	"crash", "crashes", "broken", "incorrect", "revert", "reverts", "reverted",
	"workaround", "defect", "defects", "segfault", "panic", "leak", "leaks",
	"race", "deadlock", "off-by-one", "correct", "corrects", "corrected",
}, "|") + `)\b`)

// looksLikeFix classifies a commit subject. It is a heuristic: it will miss
// fixes described neutrally and occasionally catch a feature commit that
// mentions a bug. The score treats it as evidence, not proof.
func looksLikeFix(subject string) bool {
	return fixPattern.MatchString(subject)
}

// MineDefectHistory reads git history and records which files fixes touch.
// A zero or negative sinceDays reads the whole history.
func MineDefectHistory(workspace string, sinceDays int) (DefectHistory, error) {
	h := DefectHistory{
		Fixes: map[string]int{}, Churn: map[string]int{},
		Authors: map[string]int{}, LastFix: map[string]time.Time{},
		Window: time.Duration(sinceDays) * 24 * time.Hour,
	}

	args := []string{"log", "--no-merges", "--pretty=format:%x01%an%x00%ct%x00%s", "--name-only"}
	if sinceDays > 0 {
		args = append(args, "--since="+strconv.Itoa(sinceDays)+" days ago")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		return h, err
	}

	authorsByFile := map[string]map[string]bool{}
	// \x01 starts each commit record, so splitting on it yields one block per
	// commit regardless of how many files it touched.
	for _, block := range strings.Split(string(out), "\x01") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		header, files, _ := strings.Cut(block, "\n")
		parts := strings.SplitN(header, "\x00", 3)
		if len(parts) < 3 {
			continue
		}
		author, subject := parts[0], parts[2]
		when := time.Time{}
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			when = time.Unix(ts, 0)
		}
		isFix := looksLikeFix(subject)
		if isFix {
			h.TotalFixes++
		}

		for _, f := range strings.Split(files, "\n") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			h.Churn[f]++
			if authorsByFile[f] == nil {
				authorsByFile[f] = map[string]bool{}
			}
			authorsByFile[f][author] = true
			if isFix {
				h.Fixes[f]++
				if when.After(h.LastFix[f]) {
					h.LastFix[f] = when
				}
			}
		}
	}
	for f, set := range authorsByFile {
		h.Authors[f] = len(set)
	}
	return h, nil
}

// Risk is one file's predicted defect proneness, with the evidence behind it.
type Risk struct {
	File    string   `json:"file"`
	Score   float64  `json:"score"` // 0..1
	Fixes   int      `json:"fixes"`
	Churn   int      `json:"churn"`
	Authors int      `json:"authors"`
	FanIn   int      `json:"fan_in"`
	Reasons []string `json:"reasons"`
}

// Feature weights. Prior fixes dominate because repeated repair is the
// strongest signal that code is hard to get right; the others adjust.
const (
	weightFixes   = 0.45 // how often this file has been fixed
	weightDensity = 0.25 // what share of its commits were fixes
	weightFanIn   = 0.20 // how much depends on it, from the graph
	weightAuthors = 0.10 // how many people have touched it
)

// DefectRisk ranks files by predicted defect proneness.
func (kg *KnowledgeGraph) DefectRisk(h DefectHistory, limit int) []Risk {
	// Normalise against the busiest file so scores stay comparable within a
	// repository rather than against an absolute scale that means nothing.
	maxFixes, maxAuthors, maxFanIn := 1, 1, 1
	fanIn := map[string]int{}
	for _, n := range kg.FindByType(core.KGNodeFile) {
		// Fan-in is how many other files reference the symbols this one
		// defines. Counting the file's own outgoing `references` edges would
		// measure fan-OUT — what it depends on, not what depends on it.
		count := 0
		for _, e := range kg.GetEdges(n.ID) {
			if e.Relation != core.KGRelDefines || e.From != n.ID {
				continue
			}
			for _, re := range kg.GetEdges(e.To) {
				if re.Relation == core.KGRelReferences && re.From != n.ID {
					count++
				}
			}
		}
		fanIn[n.Label] = count
		if count > maxFanIn {
			maxFanIn = count
		}
	}
	for _, n := range h.Fixes {
		if n > maxFixes {
			maxFixes = n
		}
	}
	for _, n := range h.Authors {
		if n > maxAuthors {
			maxAuthors = n
		}
	}

	var out []Risk
	for file, fixes := range h.Fixes {
		if fixes == 0 || isTestFile(file) {
			continue
		}
		churn := h.Churn[file]
		density := 0.0
		if churn > 0 {
			density = float64(fixes) / float64(churn)
		}
		r := Risk{
			File: file, Fixes: fixes, Churn: churn,
			Authors: h.Authors[file], FanIn: fanIn[file],
		}
		r.Score = weightFixes*float64(fixes)/float64(maxFixes) +
			weightDensity*density +
			weightFanIn*float64(r.FanIn)/float64(maxFanIn) +
			weightAuthors*float64(r.Authors)/float64(maxAuthors)

		r.Reasons = append(r.Reasons, strconv.Itoa(fixes)+" prior fix commit(s)")
		if density >= 0.4 && churn >= 3 {
			r.Reasons = append(r.Reasons, "most changes to it have been repairs")
		}
		if r.FanIn > 0 {
			r.Reasons = append(r.Reasons, strconv.Itoa(r.FanIn)+" references from other files")
		}
		if r.Authors >= 4 {
			r.Reasons = append(r.Reasons, strconv.Itoa(r.Authors)+" different authors")
		}
		if !h.LastFix[file].IsZero() && time.Since(h.LastFix[file]) < 30*24*time.Hour {
			r.Reasons = append(r.Reasons, "fixed within the last 30 days")
			r.Score += 0.05
		}
		if r.Score > 1 {
			r.Score = 1
		}
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].File < out[j].File
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// RankRootCauses ranks likely culprits for a failure observed in the given
// files, combining each candidate's defect history with its graph distance
// from the failure.
//
// Distance matters as much as history: a file that is fixed constantly but
// unreachable from the failing test is not the cause. Combining the two is
// what turns "here are the risky files" into "here is where to look now".
func (kg *KnowledgeGraph) RankRootCauses(h DefectHistory, failing []string, limit int) []Risk {
	risk := map[string]Risk{}
	for _, r := range kg.DefectRisk(h, 0) {
		risk[r.File] = r
	}

	// Breadth-first from the failing files, recording how many hops away each
	// reachable file is.
	distance := map[string]int{}
	frontier := map[string]bool{}
	for _, f := range failing {
		id := normalizeFileID(f)
		distance[id] = 0
		frontier[id] = true
	}
	for hop := 1; hop <= 3 && len(frontier) > 0; hop++ {
		next := map[string]bool{}
		for id := range frontier {
			for _, e := range kg.GetEdges(id) {
				for _, other := range []string{e.From, e.To} {
					if other == id || !strings.HasPrefix(other, "file:") {
						continue
					}
					if _, seen := distance[other]; !seen {
						distance[other] = hop
						next[other] = true
					}
				}
			}
			// Reach files through the symbols this one defines or references.
			for _, e := range kg.GetEdges(id) {
				if e.Relation != core.KGRelDefines && e.Relation != core.KGRelReferences {
					continue
				}
				symbol := e.To
				for _, se := range kg.GetEdges(symbol) {
					if f := se.From; strings.HasPrefix(f, "file:") {
						if _, seen := distance[f]; !seen {
							distance[f] = hop
							next[f] = true
						}
					}
				}
			}
		}
		frontier = next
	}

	var out []Risk
	for id, hops := range distance {
		file := strings.TrimPrefix(id, "file:")
		r, known := risk[file]
		if !known {
			r = Risk{File: file}
		}
		// Proximity halves per hop, so a distant high-risk file ranks below a
		// close one with a comparable history.
		proximity := 1.0 / float64(int(1)<<uint(hops))
		r.Score = (r.Score + 0.1) * proximity
		r.Reasons = append(r.Reasons, strconv.Itoa(hops)+" hop(s) from the failure")
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].File < out[j].File
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

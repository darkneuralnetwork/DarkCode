package candidate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubTrial reports a fixed outcome per patch id.
func stubTrial(outcomes map[string]Trial) TrialFunc {
	return func(_ context.Context, p Patch) Trial { return outcomes[p.ID] }
}

// The rule the whole package exists to enforce: evidence beats appearance.
func TestVerifiedPatchBeatsATidierUnverifiedOne(t *testing.T) {
	r := &Ranker{Trial: stubTrial(map[string]Trial{
		"tidy":    {Applied: true, Verified: false, Output: "FAIL: 3 tests"},
		"working": {Applied: true, Verified: true},
	})}

	// "tidy" is smaller in every structural respect and still must lose.
	scores, err := r.Rank(context.Background(), []Patch{
		{ID: "tidy", Files: map[string]string{"a.go": "x"}},
		{ID: "working", Files: map[string]string{"a.go": strings.Repeat("y", 5000), "b.go": "z"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if scores[0].Patch.ID != "working" {
		t.Errorf("winner = %q, want the one that passed the verifier", scores[0].Patch.ID)
	}
	best, ok := Best(scores)
	if !ok || best.Patch.ID != "working" {
		t.Errorf("Best = %q (keepable=%v), want working/true", best.Patch.ID, ok)
	}
}

// A patch that will not apply is worse than one that applies and fails.
func TestBrokenPatchRanksBelowAFailingOne(t *testing.T) {
	r := &Ranker{Trial: stubTrial(map[string]Trial{
		"broken":  {Applied: false, Err: "no such file"},
		"failing": {Applied: true, Verified: false},
	})}
	scores, _ := r.Rank(context.Background(), []Patch{
		{ID: "broken"}, {ID: "failing"},
	})
	if scores[0].Patch.ID != "failing" {
		t.Errorf("order = %q first, want the applying candidate", scores[0].Patch.ID)
	}
	if scores[0].Tier != TierUnverified || scores[1].Tier != TierBroken {
		t.Errorf("tiers = %d, %d; want unverified then broken", scores[0].Tier, scores[1].Tier)
	}
}

// Presenting the least bad of several failures as an answer is how an agent
// talks itself into shipping something broken.
func TestBestRefusesWhenNothingVerified(t *testing.T) {
	r := &Ranker{Trial: stubTrial(map[string]Trial{
		"a": {Applied: true, Verified: false},
		"b": {Applied: true, Verified: false},
	})}
	scores, _ := r.Rank(context.Background(), []Patch{{ID: "a"}, {ID: "b"}})
	if _, ok := Best(scores); ok {
		t.Error("Best reported a keepable winner when nothing passed the verifier")
	}
	if !strings.Contains(Format(scores), "no candidate passed") {
		t.Errorf("the summary should say nothing is keepable:\n%s", Format(scores))
	}
}

// Within a tier, the smaller change wins.
func TestChurnBreaksTiesWithinATier(t *testing.T) {
	r := &Ranker{Trial: stubTrial(map[string]Trial{
		"big":   {Applied: true, Verified: true},
		"small": {Applied: true, Verified: true},
	})}
	scores, _ := r.Rank(context.Background(), []Patch{
		{ID: "big", Files: map[string]string{"a.go": strings.Repeat("x", 900)}},
		{ID: "small", Files: map[string]string{"a.go": "x"}},
	})
	if scores[0].Patch.ID != "small" {
		t.Errorf("winner = %q, want the smaller equally-verified patch", scores[0].Patch.ID)
	}
}

// Two runs must not disagree about the winner.
func TestRankingIsTotalAndStable(t *testing.T) {
	r := &Ranker{Trial: stubTrial(map[string]Trial{
		"a": {Applied: true, Verified: true},
		"b": {Applied: true, Verified: true},
		"c": {Applied: true, Verified: true},
	})}
	patches := []Patch{
		{ID: "c", Files: map[string]string{"f": "x"}},
		{ID: "a", Files: map[string]string{"f": "x"}},
		{ID: "b", Files: map[string]string{"f": "x"}},
	}
	first, _ := r.Rank(context.Background(), patches)
	for i := 0; i < 10; i++ {
		got, _ := r.Rank(context.Background(), patches)
		for j := range got {
			if got[j].Patch.ID != first[j].Patch.ID {
				t.Fatalf("ranking changed between runs: %v vs %v", ids(got), ids(first))
			}
		}
	}
}

func ids(s []Score) []string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[i].Patch.ID
	}
	return out
}

func TestRankWithoutATrialIsAnError(t *testing.T) {
	r := &Ranker{}
	if _, err := r.Rank(context.Background(), []Patch{{ID: "a"}}); err == nil {
		t.Error("ranking without a way to verify should be refused, not guessed")
	}
}

func TestRankOfNothingIsNothing(t *testing.T) {
	r := &Ranker{Trial: stubTrial(nil)}
	got, err := r.Rank(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("empty input = %v, %v; want no scores and no error", got, err)
	}
}

// --- FileTrial ---

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, dir, rel string) (string, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return "", false
	}
	return string(body), true
}

// The requirement everything else depends on: the tree comes back unchanged.
func TestFileTrialRestoresTheWorkspace(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.txt": "original", "sub/b.txt": "also original"})

	trial := FileTrial(dir, "true")
	got := trial(context.Background(), Patch{ID: "p", Files: map[string]string{
		"a.txt":     "modified",
		"sub/b.txt": "modified too",
		"new.txt":   "created",
	}})
	if !got.Applied || !got.Verified {
		t.Fatalf("trial did not run: %+v", got)
	}

	if body, _ := readFile(t, dir, "a.txt"); body != "original" {
		t.Errorf("a.txt left as %q, want the original content", body)
	}
	if body, _ := readFile(t, dir, "sub/b.txt"); body != "also original" {
		t.Errorf("sub/b.txt left as %q", body)
	}
	if _, exists := readFile(t, dir, "new.txt"); exists {
		t.Error("a file the patch created was left behind")
	}
}

// A failing verifier must still restore, or the next candidate is judged
// against the previous one's leftovers.
func TestFileTrialRestoresAfterAFailedVerify(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.txt": "original"})

	got := FileTrial(dir, "false")(context.Background(),
		Patch{ID: "p", Files: map[string]string{"a.txt": "modified"}})
	if !got.Applied {
		t.Fatalf("patch did not apply: %+v", got)
	}
	if got.Verified {
		t.Error("a failing verifier was reported as verified")
	}
	if body, _ := readFile(t, dir, "a.txt"); body != "original" {
		t.Errorf("a.txt left as %q after a failed verify", body)
	}
}

// The verifier's exit status is the whole verdict; its output is not read for
// meaning, only kept for the report.
func TestFileTrialUsesExitStatusAndKeepsOutput(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.txt": "x"})

	// Prints a reassuring message and fails anyway.
	got := FileTrial(dir, "echo 'all good'; exit 1")(context.Background(),
		Patch{ID: "p", Files: map[string]string{"a.txt": "y"}})
	if got.Verified {
		t.Error("a non-zero exit was read as success because the output looked fine")
	}
	if !strings.Contains(got.Output, "all good") {
		t.Errorf("verifier output was not kept: %q", got.Output)
	}
}

// A model-authored path is untrusted input.
func TestFileTrialRefusesToEscapeTheWorkspace(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.txt": "x"})
	outside := filepath.Join(filepath.Dir(dir), "escaped.txt")
	_ = os.Remove(outside)

	got := FileTrial(dir, "true")(context.Background(), Patch{
		ID: "evil", Files: map[string]string{"../escaped.txt": "pwned"},
	})
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a patch wrote outside the workspace")
	}
	_ = got
}

func TestFileTrialNeedsAVerifier(t *testing.T) {
	got := FileTrial(t.TempDir(), "")(context.Background(), Patch{ID: "p"})
	if got.Applied || got.Verified {
		t.Errorf("a trial with no verifier claimed a result: %+v", got)
	}
	if got.Err == "" {
		t.Error("the refusal should say why")
	}
}

// A patch that cannot be fully written must leave nothing behind.
func TestPartialApplyIsFullyReverted(t *testing.T) {
	dir := writeTree(t, map[string]string{"ok.txt": "original"})
	// The second path escapes, so the first write must be undone.
	undo, err := applyPatch(dir, Patch{ID: "p", Files: map[string]string{
		"ok.txt":        "modified",
		"../escape.txt": "nope",
	}})
	undo()
	if err == nil {
		t.Fatal("the escaping path was accepted")
	}
	if body, _ := readFile(t, dir, "ok.txt"); body != "original" {
		t.Errorf("ok.txt left as %q after a partial apply", body)
	}
}

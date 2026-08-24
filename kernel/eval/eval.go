// Package eval scores retrieval quality against a labelled corpus.
//
// # WHY THIS EXISTS
//
// The claim this project rests on is that a confidence-weighted graph over the
// repository makes recall better. Until now that was an argument. The retrieval
// code had two micro-benchmarks measuring how *fast* fusion runs and nothing at
// all measuring whether it finds the right thing, so every change to ranking
// was defended by reading the diff.
//
// A competitor can print "R@5 95.2%". DarkCode could print an explanation. That
// asymmetry is the gap, and it is not closed by writing a better explanation.
//
// # WHAT IT MEASURES, AND WHAT IT DELIBERATELY DOES NOT
//
// The interesting question is not "is hybrid retrieval good" — that is settled
// in the literature and darkcode's fusion is the standard RRF. The question
// specific to this project is **what the knowledge graph is worth**, because
// that is the thing nobody else has. So the two adapters that always run differ
// in exactly one variable:
//
//	keyword        NewHybridRetriever(mem, nil)  — no graph
//	keyword+graph  NewHybridRetriever(mem, kg)   — graph attached
//
// Both run offline, with no embedder and therefore no model call, no API key
// and no cost. The difference between their scores is the graph's contribution,
// isolated. A third adapter adds the vector stream and runs only when an
// embedder is configured, because a benchmark that needs a paid key is a
// benchmark nobody reruns.
//
// # WHY A CORPUS ON DISK
//
// Same reason as package bench: a benchmark whose cases live in Go is a
// benchmark only its author can extend, and gold labels that are code review
// themselves. A corpus is JSON — entries, queries, and the ids that should come
// back — so disagreeing with a label is a pull request rather than an argument.
//
// No model grades anything. A query is right when the gold id is in the top k,
// which is a set membership test.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/memory/memory"
	"github.com/darkcode/memory/recall"
)

// Entry is one item placed in memory before the queries run.
//
// Kind selects the tier: "episodic" for something that happened, "semantic" for
// a durable fact. The distinction is not cosmetic — they are retrieved from
// different stores and a corpus that puts everything in one tier would only
// measure half the system.
type Entry struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Text    string   `json:"text"`
	Content string   `json:"content,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	// Node attaches a graph node for this entry, so a corpus can exercise the
	// signal the graph adapter exists to measure. Optional.
	Node *Node `json:"node,omitempty"`
}

// Node is a graph fact about an entry: a file, a symbol, a package.
type Node struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// Query is one question and the entry ids that answer it.
type Query struct {
	Q string `json:"q"`
	// Gold lists every entry that is a correct answer. More than one is normal
	// and is what separates recall from precision.
	Gold []string `json:"gold"`
	// Note records why these are the gold answers, so a label can be argued
	// with rather than taken on trust.
	Note string `json:"note,omitempty"`
}

// Corpus is one labelled dataset.
type Corpus struct {
	Name    string  `json:"name"`
	About   string  `json:"about"`
	Entries []Entry `json:"entries"`
	Queries []Query `json:"queries"`
}

// Score is one adapter's result over a corpus.
type Score struct {
	Adapter string
	Queries int
	// R@k — the share of gold answers that appeared in the top k. This is the
	// metric that matters for an agent: a fact it never sees cannot help it.
	R1, R5, R10 float64
	// P5 — the share of the top 5 that was gold. Bounded above by the corpus's
	// gold density, so compare it across adapters and never against 1.
	P5 float64
	// MRR — 1/rank of the first gold answer, averaged. Sensitive to ordering in
	// a way recall is not.
	MRR float64
	// Misses names the queries that returned no gold answer at all, because the
	// aggregate hides exactly the cases worth reading.
	Misses []string
	// Signals counts which streams produced the gold hits, so the vector and
	// graph streams can be shown to earn their cost rather than assumed to.
	Signals map[string]int
}

// Load reads a corpus directory.
func Load(dir string) (*Corpus, error) {
	b, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		return nil, err
	}
	var c Corpus
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if c.Name == "" {
		c.Name = filepath.Base(dir)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate rejects a corpus that cannot measure anything — a gold id with no
// entry scores zero for every adapter forever and looks like a retrieval bug.
func (c *Corpus) validate() error {
	ids := map[string]bool{}
	for _, e := range c.Entries {
		if e.ID == "" {
			return fmt.Errorf("%s: an entry has no id", c.Name)
		}
		if ids[e.ID] {
			return fmt.Errorf("%s: duplicate entry id %q", c.Name, e.ID)
		}
		ids[e.ID] = true
	}
	if len(c.Queries) == 0 {
		return fmt.Errorf("%s: no queries", c.Name)
	}
	for _, q := range c.Queries {
		if len(q.Gold) == 0 {
			return fmt.Errorf("%s: query %q has no gold answer", c.Name, q.Q)
		}
		for _, g := range q.Gold {
			if !ids[g] {
				return fmt.Errorf("%s: query %q cites gold id %q, which no entry has", c.Name, q.Q, g)
			}
		}
	}
	return nil
}

// Build loads the corpus into a fresh memory system rooted at dir.
//
// The returned system owns a real knowledge graph holding whatever nodes the
// corpus declared, so the graph adapter has something to traverse. Callers must
// Shutdown it.
//
// Entries are written through the recall gateway rather than into the stores
// directly. That is not ceremony: placement is the gateway's decision, and a
// benchmark that populated memory by a route the agent never uses would be
// measuring a store the agent does not actually write.
func Build(c *Corpus, dir string) (*memory.System, error) {
	sys, err := memory.NewSystem(dir)
	if err != nil {
		return nil, err
	}
	rec, err := recall.New(sys)
	if err != nil {
		sys.Shutdown()
		return nil, err
	}
	for _, e := range c.Entries {
		var f recall.Fact
		switch e.Kind {
		case "semantic":
			f = recall.Note{Key: e.ID, Content: body(e), Category: "reference", Tags: recall.SortedTags(e.Tags)}
		default:
			f = recall.Event{Entry: core.EpisodicEntry{
				ID:        e.ID,
				TaskGoal:  e.Text,
				Summary:   e.Text,
				Output:    e.Content,
				Outcome:   "success",
				Timestamp: corpusTime(),
			}}
		}
		if err := rec.Remember(f); err != nil {
			sys.Shutdown()
			return nil, fmt.Errorf("entry %s: %w", e.ID, err)
		}
		if e.Node != nil {
			if err := rec.Remember(recall.Entity{Node: &core.KGNode{
				ID: e.Node.ID, Label: e.Node.Label,
				Type: core.KGNodeType(e.Node.Type), Confidence: 1,
				Properties: map[string]string{"origin": "code_index"},
			}}); err != nil {
				sys.Shutdown()
				return nil, fmt.Errorf("entry %s node: %w", e.ID, err)
			}
		}
	}
	return sys, nil
}

// corpusTime is a fixed point in the recent past for every entry.
//
// Recall breaks ties on recency, so entries added "now" in loop order would
// score by insertion order and the benchmark would measure the corpus file's
// line ordering. One timestamp for all of them removes that variable — this
// harness is about relevance, and a corpus about time would need its own ages
// declared per entry rather than inherited from when the test ran.
func corpusTime() time.Time { return time.Now().Add(-time.Hour) }

// body is what a semantic entry stores: the question-shaped text and its
// content, so keyword matching sees both.
func body(e Entry) string {
	if e.Content == "" {
		return e.Text
	}
	return e.Text + "\n" + e.Content
}

// Retriever is what an adapter provides. Satisfied by *memory.HybridRetriever.
type Retriever interface {
	Recall(query string, k int) []memory.RecallHit
}

// Run scores one adapter over the corpus.
//
// k is the depth the ranked list is cut at; 10 covers R@10 and everything
// shallower, which is every metric here.
func Run(name string, c *Corpus, r Retriever) Score {
	s := Score{Adapter: name, Queries: len(c.Queries), Signals: map[string]int{}}
	for _, q := range c.Queries {
		gold := map[string]bool{}
		for _, g := range q.Gold {
			gold[g] = true
		}
		hits := r.Recall(q.Q, 10)

		firstRank, foundTop5 := 0, 0
		var found1, found5, found10 int
		for i, h := range hits {
			if !gold[h.ID] {
				continue
			}
			if firstRank == 0 {
				firstRank = i + 1
			}
			if i < 1 {
				found1++
			}
			if i < 5 {
				found5++
				foundTop5++
				s.Signals[signalOf(h)]++
			}
			if i < 10 {
				found10++
			}
		}
		n := float64(len(q.Gold))
		s.R1 += float64(found1) / n
		s.R5 += float64(found5) / n
		s.R10 += float64(found10) / n
		s.P5 += float64(foundTop5) / 5
		if firstRank > 0 {
			s.MRR += 1 / float64(firstRank)
		} else {
			s.Misses = append(s.Misses, q.Q)
		}
	}
	n := float64(len(c.Queries))
	s.R1, s.R5, s.R10 = s.R1/n, s.R5/n, s.R10/n
	s.P5, s.MRR = s.P5/n, s.MRR/n
	sort.Strings(s.Misses)
	return s
}

func signalOf(h memory.RecallHit) string {
	if h.Signal == "" {
		return "unattributed"
	}
	return h.Signal
}

// Scorecard renders results as a table, so a run can be pasted into a report
// without anyone reformatting numbers by hand.
func Scorecard(c *Corpus, scores []Score) string {
	var b strings.Builder
	fmt.Fprintf(&b, "corpus: %s — %d entries, %d queries\n", c.Name, len(c.Entries), len(c.Queries))
	if c.About != "" {
		fmt.Fprintf(&b, "%s\n", c.About)
	}
	fmt.Fprintf(&b, "\n%-16s %7s %7s %7s %7s %7s  %s\n", "adapter", "R@1", "R@5", "R@10", "P@5", "MRR", "misses")
	for _, s := range scores {
		fmt.Fprintf(&b, "%-16s %7.3f %7.3f %7.3f %7.3f %7.3f  %d\n",
			s.Adapter, s.R1, s.R5, s.R10, s.P5, s.MRR, len(s.Misses))
	}
	for _, s := range scores {
		if len(s.Misses) == 0 && len(s.Signals) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s:\n", s.Adapter)
		for _, sig := range sortedKeys(s.Signals) {
			fmt.Fprintf(&b, "  signal %-18s %d gold hit(s) in the top 5\n", sig, s.Signals[sig])
		}
		for _, m := range s.Misses {
			fmt.Fprintf(&b, "  missed: %s\n", m)
		}
	}
	return b.String()
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

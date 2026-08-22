package recall

import (
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// fakeStore records where each fact landed, so the tests assert PLACEMENT
// rather than re-testing the stores themselves.
type fakeStore struct {
	semantic   []string
	episodic   []core.EpisodicEntry
	procedural []string
	kg         *fakeKG
	err        error
}

type fakeKG struct {
	nodes []*core.KGNode
	links [][3]string
}

func (k *fakeKG) AddNode(n *core.KGNode) error { k.nodes = append(k.nodes, n); return nil }
func (k *fakeKG) AddEdge(e *core.KGEdge) error {
	k.links = append(k.links, [3]string{e.From, string(e.Relation), e.To})
	return nil
}
func (k *fakeKG) GetNode(string) (*core.KGNode, bool) { return nil, false }
func (k *fakeKG) Relate(from, to string, r core.KGRelationType) error {
	k.links = append(k.links, [3]string{from, string(r), to})
	return nil
}
func (k *fakeKG) RecordWordRelations(string) error          { return nil }
func (k *fakeKG) AllNodes() []*core.KGNode                  { return k.nodes }
func (k *fakeKG) FindByType(core.KGNodeType) []*core.KGNode { return nil }
func (k *fakeKG) GetEdges(string) []*core.KGEdge            { return nil }
func (k *fakeKG) AllEdges() []*core.KGEdge                  { return nil }
func (k *fakeKG) Stats() (int, int)                         { return len(k.nodes), 0 }
func (k *fakeKG) ConceptRelations(string) interface{}       { return nil }
func (k *fakeKG) AdjustConfidence(string, float64, float64) (float64, bool) {
	return 0, false
}

func (f *fakeStore) SemanticAdd(key, content, category string, tags []string) error {
	if f.err != nil {
		return f.err
	}
	f.semantic = append(f.semantic, key)
	return nil
}
func (f *fakeStore) EpisodicAdd(e core.EpisodicEntry) error {
	f.episodic = append(f.episodic, e)
	return nil
}
func (f *fakeStore) ProceduralAdd(s *core.Skill) error {
	f.procedural = append(f.procedural, s.Name)
	return nil
}
func (f *fakeStore) KG() core.KnowledgeGraphStore { return f.kg }

func newManager(t *testing.T) (*Manager, *fakeStore) {
	t.Helper()
	fs := &fakeStore{kg: &fakeKG{}}
	m, err := New(fs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, fs
}

// TestShapeDecidesTheStore is the whole point: a caller states what it learned,
// not where to put it. Placement was thirty-two independent decisions that had
// to agree and had no way to.
func TestShapeDecidesTheStore(t *testing.T) {
	m, fs := newManager(t)

	if err := m.RememberAll(
		Entity{Node: &core.KGNode{ID: "file:a.go", Label: "a.go", Type: core.KGNodeFile}},
		Relate("file:a.go", "pkg:main", core.KGRelContains),
		Note{Key: "note:1", Content: "the retry layer backs off on 429", Category: "design"},
		Event{Entry: core.EpisodicEntry{TaskGoal: "fix the test", Summary: "fixed", Outcome: "success"}},
		Procedure{Skill: &core.Skill{Name: "run-tests"}},
	); err != nil {
		t.Fatalf("RememberAll: %v", err)
	}

	if len(fs.kg.nodes) != 1 {
		t.Errorf("entity did not reach the graph: %d nodes", len(fs.kg.nodes))
	}
	if len(fs.kg.links) != 1 {
		t.Errorf("link did not reach the graph: %d links", len(fs.kg.links))
	}
	if len(fs.semantic) != 1 {
		t.Errorf("note did not reach semantic memory: %d", len(fs.semantic))
	}
	if len(fs.episodic) != 1 {
		t.Errorf("event did not reach episodic memory: %d", len(fs.episodic))
	}
	if len(fs.procedural) != 1 {
		t.Errorf("procedure did not reach procedural memory: %d", len(fs.procedural))
	}
}

// TestEntityPropertiesSurvive — an earlier design flattened everything into a
// subject-verb-object triple. That is a cleaner API that silently drops the
// properties the graph answers questions from.
func TestEntityPropertiesSurvive(t *testing.T) {
	m, fs := newManager(t)
	props := map[string]string{"language": "go", "symbols": "12", "content_hash": "abc"}

	if err := m.Remember(Entity{Node: &core.KGNode{
		ID: "file:a.go", Label: "a.go", Type: core.KGNodeFile,
		Properties: props, Provenance: "a.go:1", Confidence: 0.9,
	}}); err != nil {
		t.Fatal(err)
	}

	got := fs.kg.nodes[0]
	for k, v := range props {
		if got.Properties[k] != v {
			t.Errorf("property %q lost: got %q want %q", k, got.Properties[k], v)
		}
	}
	if got.Provenance != "a.go:1" || got.Confidence != 0.9 {
		t.Error("provenance or confidence was dropped")
	}
}

// TestSameFactLearnedTwiceIsOneAddress — deduplication only works if identity
// is a property of the fact rather than of who wrote it.
func TestSameFactLearnedTwiceIsOneAddress(t *testing.T) {
	a := Relate("task:1", "file:a.go", core.KGRelContains)
	b := Relate("task:1", "file:a.go", core.KGRelContains)
	if Address(a) != Address(b) {
		t.Error("the same link produced two addresses")
	}
	c := Relate("task:1", "file:b.go", core.KGRelContains)
	if Address(a) == Address(c) {
		t.Error("different links collided onto one address")
	}
}

// TestAddressIgnoresRestampedFields — re-reading a file restamps confidence,
// provenance and timestamps. If those were part of identity, every pass would
// double the store.
func TestAddressIgnoresRestampedFields(t *testing.T) {
	first := Entity{Node: &core.KGNode{ID: "file:a.go", Confidence: 0.5, Provenance: "a.go:1"}}
	later := Entity{Node: &core.KGNode{ID: "file:a.go", Confidence: 1.0, Provenance: "a.go:99",
		Properties: map[string]string{"content_hash": "changed"}}}
	if Address(first) != Address(later) {
		t.Error("re-reading a file produced a second address — every index pass would double the store")
	}
}

// TestNoteAddressFoldsPhrasing — two writers phrasing the same note
// differently must not produce two entries.
func TestNoteAddressFoldsPhrasing(t *testing.T) {
	a := Note{Content: "The Retry Layer   backs off"}
	b := Note{Content: "the retry layer backs off"}
	if Address(a) != Address(b) {
		t.Error("whitespace and case produced two addresses for one note")
	}
}

func TestKeyedNotesAddressByKey(t *testing.T) {
	a := Note{Key: "design:retry", Content: "one wording"}
	b := Note{Key: "design:retry", Content: "a completely different wording"}
	if Address(a) != Address(b) {
		t.Error("a keyed note changed address when its content changed — " +
			"re-learning it would append instead of replacing")
	}
}

// TestUnkeyedNoteStillGetsAKey — SemanticAdd is keyed, so an unkeyed note
// needs one; using the content address means writing it twice is idempotent.
func TestUnkeyedNoteStillGetsAKey(t *testing.T) {
	m, fs := newManager(t)
	n := Note{Content: "some observation", Category: "obs"}
	if err := m.Remember(n); err != nil {
		t.Fatal(err)
	}
	if err := m.Remember(n); err != nil {
		t.Fatal(err)
	}
	if len(fs.semantic) != 2 {
		t.Fatalf("expected two writes, got %d", len(fs.semantic))
	}
	if fs.semantic[0] != fs.semantic[1] {
		t.Errorf("the same note wrote two keys: %q and %q", fs.semantic[0], fs.semantic[1])
	}
	if !strings.HasPrefix(fs.semantic[0], "note:") {
		t.Errorf("unkeyed note got key %q, want a content address", fs.semantic[0])
	}
}

func TestEmptyNoteIsNotAnError(t *testing.T) {
	m, fs := newManager(t)
	if err := m.Remember(Note{Content: "   "}); err != nil {
		t.Errorf("an empty note errored: %v", err)
	}
	if len(fs.semantic) != 0 {
		t.Error("an empty note was written")
	}
}

func TestMalformedFactsAreRefused(t *testing.T) {
	m, _ := newManager(t)
	for name, f := range map[string]Fact{
		"entity without a node":   Entity{},
		"entity without an id":    Entity{Node: &core.KGNode{}},
		"link with no target":     Link{Edge: &core.KGEdge{From: "a"}},
		"procedure with no skill": Procedure{},
	} {
		if err := m.Remember(f); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestRememberAllKeepsGoing — a structural sync produces hundreds of facts and
// one unwritable entry must not abandon the rest.
func TestRememberAllKeepsGoing(t *testing.T) {
	m, fs := newManager(t)
	err := m.RememberAll(
		Entity{}, // fails
		Note{Key: "k", Content: "kept"},
		Event{Entry: core.EpisodicEntry{TaskGoal: "g"}},
	)
	if err == nil {
		t.Error("the failure was not reported")
	}
	if len(fs.semantic) != 1 || len(fs.episodic) != 1 {
		t.Error("a single bad fact abandoned the rest of the batch")
	}
}

func TestNilStoreIsRefused(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("New(nil) returned a manager whose every write is a silent no-op")
	}
}

// TestGraphAdapterRoutesEveryWriteThroughTheManager is the proof that the
// adapter delivers the gateway property. The graph sync keeps calling
// AddNode/AddEdge — rewriting its ten multi-line composite literals would be
// churn with no behavioural payoff — so what matters is that those calls land
// in the manager, not what they are spelled.
func TestGraphAdapterRoutesEveryWriteThroughTheManager(t *testing.T) {
	fs := &fakeStore{kg: &fakeKG{}}
	m, err := New(fs)
	if err != nil {
		t.Fatal(err)
	}
	w := Graph(m)
	if w == nil {
		t.Fatal("Graph returned nil for a usable manager")
	}

	if err := w.AddNode(&core.KGNode{ID: "file:a.go", Label: "a.go", Type: core.KGNodeFile,
		Properties: map[string]string{"language": "go"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.AddEdge(&core.KGEdge{From: "file:a.go", To: "sym:F",
		Relation: core.KGRelDefines, Weight: 1.0, Provenance: "a.go:12"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Relate("file:a.go", "pkg:main", core.KGRelContains); err != nil {
		t.Fatal(err)
	}

	if len(fs.kg.nodes) != 1 {
		t.Errorf("AddNode did not reach the store: %d", len(fs.kg.nodes))
	}
	if fs.kg.nodes[0].Properties["language"] != "go" {
		t.Error("the adapter dropped a node property")
	}
	if len(fs.kg.links) != 2 {
		t.Errorf("edges did not reach the store: %d, want 2", len(fs.kg.links))
	}
}

// TestGraphAdapterReadsPassThrough — this manager owns WRITES. Read routing is
// the data-source layer's job, and putting retrieval policy in the fact store
// would conflate the two.
func TestGraphAdapterReadsPassThrough(t *testing.T) {
	fs := &fakeStore{kg: &fakeKG{}}
	m, _ := New(fs)
	w := Graph(m)

	_ = w.AddNode(&core.KGNode{ID: "n1"})
	if got := w.AllNodes(); len(got) != 1 || got[0].ID != "n1" {
		t.Errorf("reads did not reach the underlying graph: %+v", got)
	}
	if n, _ := w.Stats(); n != 1 {
		t.Errorf("Stats did not pass through: %d", n)
	}
}

func TestGraphIsNilWithoutAStore(t *testing.T) {
	if Graph(nil) != nil {
		t.Error("Graph(nil) returned a writer that would panic on use")
	}
	if Graph(&Manager{}) != nil {
		t.Error("Graph on a zero manager returned a writer")
	}
}

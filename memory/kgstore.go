package memory

// kgstore.go — incremental persistence for the knowledge graph.
//
// WHY THIS EXISTS
//
// The graph used to be a single JSON document rewritten in full on a 2-second
// debounce. Measured against the real store on a developer machine:
//
//	knowledge_graph.json   3.6 MB
//	  marshal (every 2s under RLock)   173 ms
//	  unmarshal (blocking at startup)  135 ms
//
// That is roughly 9% of a core burned continuously during any active session,
// while holding a lock that readers need — and it scales linearly, so a 36 MB
// graph would spend 1.7s of every 2s serialising itself. It was the system's
// largest real performance cost, well ahead of the retrieval scan it was
// commonly blamed on (~12 ms for 50,000 vectors).
//
// SQLite does not make that write faster. It makes it not happen: a node
// changes, one row is written. There is no whole-graph pass at any point.
//
// WHAT THIS DELIBERATELY DOES NOT DO
//
// The graph is still held in memory for reads, and every query method is
// unchanged. This fixes the write amplification, not the resident-set ceiling.
// Streaming queries out of SQL is the larger change that belongs with the
// recall port; doing it here would mean rewriting fifty query methods to close
// a cost nobody has measured yet.
//
// Vectors move from JSON number arrays to BLOBs on the way, which is ~3x
// smaller for the largest field in the store.
//
// WHY ONLY THE GRAPH
//
// episodic.json, semantic.json and procedural.json still use the debounced
// whole-file writer, and that is a measured decision rather than an oversight:
//
//	episodic.json    320 KB   marshal 3 ms
//	semantic.json    393 KB   marshal 4 ms
//	procedural.json   23 KB   marshal 1 ms
//
// Roughly 9 ms every two seconds between them — 0.45% of a core, against the
// graph's 9%. Two orders of magnitude apart, and not worth a migration today.
// Revisit if those files reach a few megabytes, where the same linear cost
// starts to bite; the pattern to copy is right here.

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, still one static binary

	"github.com/darkcode/core"
)

// kgStore is the SQLite-backed persistence layer for KnowledgeGraph.
type kgStore struct {
	db *sql.DB
}

const kgSchema = `
CREATE TABLE IF NOT EXISTS nodes (
	id          TEXT PRIMARY KEY,
	label       TEXT NOT NULL,
	type        TEXT NOT NULL,
	properties  TEXT,
	vector      BLOB,
	created_at  INTEGER NOT NULL,
	provenance  TEXT,
	confidence  REAL,
	last_seen   INTEGER
);
CREATE INDEX IF NOT EXISTS nodes_type ON nodes(type);

CREATE TABLE IF NOT EXISTS edges (
	from_id     TEXT NOT NULL,
	to_id       TEXT NOT NULL,
	relation    TEXT NOT NULL,
	weight      REAL,
	created_at  INTEGER NOT NULL,
	provenance  TEXT,
	PRIMARY KEY (from_id, to_id, relation)
);
CREATE INDEX IF NOT EXISTS edges_from ON edges(from_id);
CREATE INDEX IF NOT EXISTS edges_to   ON edges(to_id);
`

// openKGStore opens (creating if needed) the graph database at path.
func openKGStore(path string) (*kgStore, error) {
	// WAL is what makes per-mutation writes cheap: a commit appends to the log
	// instead of rewriting pages. NORMAL synchronous trades an fsync per commit
	// for durability only against OS crash, not process crash — the right trade
	// for a derived cache that a re-index can rebuild.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open knowledge graph db: %w", err)
	}
	// One writer. SQLite serialises writes anyway, and letting the pool open
	// several connections only produces SQLITE_BUSY to retry.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(kgSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create knowledge graph schema: %w", err)
	}
	return &kgStore{db: db}, nil
}

func (s *kgStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// encodeVector packs float32s little-endian. A 768-dim vector is 3 KB here
// against ~9 KB as a JSON number array, and it is the largest field in the row.
func encodeVector(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVector(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// UpsertNode writes one node. This is the call that replaces re-serialising
// the entire graph.
func (s *kgStore) UpsertNode(n *core.KGNode) error {
	if s == nil || s.db == nil || n == nil {
		return nil
	}
	var props []byte
	if len(n.Properties) > 0 {
		props, _ = json.Marshal(n.Properties)
	}
	_, err := s.db.Exec(`
		INSERT INTO nodes (id,label,type,properties,vector,created_at,provenance,confidence,last_seen)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			label=excluded.label, type=excluded.type, properties=excluded.properties,
			vector=excluded.vector, provenance=excluded.provenance,
			confidence=excluded.confidence, last_seen=excluded.last_seen`,
		n.ID, n.Label, string(n.Type), string(props), encodeVector(n.Vector),
		n.CreatedAt.UnixNano(), n.Provenance, n.Confidence, n.LastSeen.UnixNano())
	return err
}

func (s *kgStore) UpsertEdge(e *core.KGEdge) error {
	if s == nil || s.db == nil || e == nil {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO edges (from_id,to_id,relation,weight,created_at,provenance)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(from_id,to_id,relation) DO UPDATE SET
			weight=excluded.weight, provenance=excluded.provenance`,
		e.From, e.To, string(e.Relation), e.Weight, e.CreatedAt.UnixNano(), e.Provenance)
	return err
}

// DeleteNode removes a node and every edge touching it, in one transaction so
// the graph can never be observed with a dangling edge.
func (s *kgStore) DeleteNode(id string) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM edges WHERE from_id=? OR to_id=?`, id, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceAll rewrites the whole store in one transaction. Used only by the
// one-time JSON migration and by prune passes that rebuild the concept set —
// the operations that genuinely change everything at once.
func (s *kgStore) ReplaceAll(nodes []*core.KGNode, edges []*core.KGEdge) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	if _, err := tx.Exec(`DELETE FROM nodes`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM edges`); err != nil {
		return err
	}
	ns, err := tx.Prepare(`INSERT INTO nodes (id,label,type,properties,vector,created_at,provenance,confidence,last_seen) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ns.Close()
	for _, n := range nodes {
		var props []byte
		if len(n.Properties) > 0 {
			props, _ = json.Marshal(n.Properties)
		}
		if _, err := ns.Exec(n.ID, n.Label, string(n.Type), string(props), encodeVector(n.Vector),
			n.CreatedAt.UnixNano(), n.Provenance, n.Confidence, n.LastSeen.UnixNano()); err != nil {
			return err
		}
	}
	es, err := tx.Prepare(`INSERT OR REPLACE INTO edges (from_id,to_id,relation,weight,created_at,provenance) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer es.Close()
	for _, e := range edges {
		if _, err := es.Exec(e.From, e.To, string(e.Relation), e.Weight, e.CreatedAt.UnixNano(), e.Provenance); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadAll reads the graph back into memory at startup.
func (s *kgStore) LoadAll() ([]*core.KGNode, []*core.KGEdge, error) {
	if s == nil || s.db == nil {
		return nil, nil, nil
	}
	nrows, err := s.db.Query(`SELECT id,label,type,properties,vector,created_at,provenance,confidence,last_seen FROM nodes`)
	if err != nil {
		return nil, nil, err
	}
	defer nrows.Close()

	var nodes []*core.KGNode
	for nrows.Next() {
		var (
			n            core.KGNode
			typ, props   string
			vec          []byte
			created, ls  int64
			prov         sql.NullString
			confidence   sql.NullFloat64
			propsNull    sql.NullString
			provenanceOK = &prov
		)
		if err := nrows.Scan(&n.ID, &n.Label, &typ, &propsNull, &vec, &created, provenanceOK, &confidence, &ls); err != nil {
			return nil, nil, err
		}
		props = propsNull.String
		n.Type = core.KGNodeType(typ)
		if props != "" {
			_ = json.Unmarshal([]byte(props), &n.Properties)
		}
		n.Vector = decodeVector(vec)
		n.CreatedAt = time.Unix(0, created)
		n.Provenance = prov.String
		n.Confidence = confidence.Float64
		if ls > 0 {
			n.LastSeen = time.Unix(0, ls)
		}
		nodes = append(nodes, &n)
	}
	if err := nrows.Err(); err != nil {
		return nil, nil, err
	}

	erows, err := s.db.Query(`SELECT from_id,to_id,relation,weight,created_at,provenance FROM edges`)
	if err != nil {
		return nil, nil, err
	}
	defer erows.Close()

	var edges []*core.KGEdge
	for erows.Next() {
		var (
			e       core.KGEdge
			rel     string
			created int64
			weight  sql.NullFloat64
			prov    sql.NullString
		)
		if err := erows.Scan(&e.From, &e.To, &rel, &weight, &created, &prov); err != nil {
			return nil, nil, err
		}
		e.Relation = core.KGRelationType(rel)
		e.Weight = weight.Float64
		e.CreatedAt = time.Unix(0, created)
		e.Provenance = prov.String
		edges = append(edges, &e)
	}
	return nodes, edges, erows.Err()
}

// Count reports stored node and edge counts, for the migration to verify
// against what it read.
func (s *kgStore) Count() (nodes, edges int, err error) {
	if s == nil || s.db == nil {
		return 0, 0, nil
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edges)
	return
}

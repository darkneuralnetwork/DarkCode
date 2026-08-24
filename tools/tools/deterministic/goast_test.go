package deterministic

import (
	"context"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// Generic receivers reach the parser as shapes the plain Ident/StarExpr pair
// does not cover. The pointer forms panicked outright, and since indexing runs
// on a background goroutine at startup, that ended the process: opening any
// repository with a pointer method on a generic type killed the agent before it
// finished waking up.
func TestReceiverShapes(t *testing.T) {
	src := `package p

type Cache[K comparable, V any] struct{}

func (c Cache[K, V]) Len() int     { return 0 }
func (c *Cache[K, V]) Put(k K, v V) {}

type Box[T any] struct{}

func (b Box[T]) Get() T   { var z T; return z }
func (b *Box[T]) Set(t T) {}

type Plain struct{}

func (p Plain) A()  {}
func (p *Plain) B() {}

func Free() {}
`
	defs := parseDefinitions(token.NewFileSet(), goFile{path: "p.go", src: []byte(src)})

	got := map[string]string{}
	for _, d := range defs {
		if d.Kind == "function" {
			got[d.Name] = d.Receiver
		}
	}

	// A method belongs to the type that declares it whether or not that type is
	// generic; an empty receiver would orphan it in the graph.
	want := map[string]string{
		"Len": "Cache", "Put": "*Cache",
		"Get": "Box", "Set": "*Box",
		"A": "Plain", "B": "*Plain",
		"Free": "",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("receiver of %s = %q, want %q", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d functions, want %d: %v", len(got), len(want), got)
	}
}

// The panic was reached through the full file walk, not through the helper, so
// the regression is only really pinned by parsing a whole file.
func TestParseDefinitionsSurvivesUnusualReceivers(t *testing.T) {
	for name, src := range map[string]string{
		"generic pointer, several params": "package p\ntype M[K, V any] struct{}\nfunc (m *M[K, V]) F() {}\n",
		"generic pointer, one param":      "package p\ntype S[T any] struct{}\nfunc (s *S[T]) F() {}\n",
		"generic value, several params":   "package p\ntype M[K, V any] struct{}\nfunc (m M[K, V]) F() {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			defs := parseDefinitions(token.NewFileSet(), goFile{path: "p.go", src: []byte(src)})
			var f *definition
			for i := range defs {
				if defs[i].Name == "F" {
					f = &defs[i]
				}
			}
			if f == nil {
				t.Fatal("the method was not indexed at all")
			}
			if f.Receiver == "" {
				t.Error("the method was indexed with no receiver, orphaning it from its type")
			}
		})
	}
}

// The panic the user hit came through the full workspace walk, so the
// regression is only truly pinned end to end.
func TestE2EGenericWorkspaceIndexes(t *testing.T) {
	root := t.TempDir()
	src := `package g

type Cache[K comparable, V any] struct{ m map[K]V }

func (c *Cache[K, V]) Put(k K, v V) { c.m[k] = v }
func (c Cache[K, V]) Len() int      { return len(c.m) }

type Box[T any] struct{ v T }

func (b *Box[T]) Set(v T) { b.v = v }
`
	if err := os.WriteFile(filepath.Join(root, "g.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	kg := newTestKG(t)
	stats, err := SyncWorkspaceKG(context.Background(), root, kg)
	if err != nil {
		t.Fatalf("indexing a generics-only package failed: %v", err)
	}
	if stats.Symbols == 0 {
		t.Fatalf("nothing was indexed: %+v", stats)
	}
	t.Logf("indexed %+v", stats)
}

package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/darkcode/spill"
)

func registryWithSpill(t *testing.T) (*Registry, *spill.Store) {
	t.Helper()
	st, err := spill.New(t.TempDir())
	if err != nil {
		t.Fatalf("spill.New: %v", err)
	}
	r := NewRegistry()
	r.SetSpillStore(st)
	RegisterSpillTool(r, st)
	return r, st
}

// TestObserveResultOffloadsAndReadResultRetrieves is the integration: the two
// halves are useless apart. Offloading without a way to page is exactly as
// lossy as the truncation it replaced.
func TestObserveResultOffloadsAndReadResultRetrieves(t *testing.T) {
	r, _ := registryWithSpill(t)

	needle := "DEEP-MARKER-XYZ"
	content := strings.Repeat("filler line\n", 20_000) + needle + "\n" + strings.Repeat("filler line\n", 20_000)

	obs := r.ObserveResult("read_file", content)

	if len(obs) >= len(content)/10 {
		t.Errorf("observation is %d bytes for a %d-byte result — barely reduced", len(obs), len(content))
	}
	if strings.Contains(obs, needle) {
		t.Fatal("test is not exercising the omitted region")
	}

	m := regexp.MustCompile(`\[result ([0-9a-f]{16})`).FindStringSubmatch(obs)
	if m == nil {
		t.Fatalf("no retrieval handle in the observation:\n%s", obs[:200])
	}
	id := m[1]

	// Page the whole thing back through the tool the model would use.
	entry, ok := r.Get("read_result")
	if !ok {
		t.Fatal("read_result was not registered")
	}
	var got strings.Builder
	for off := 0; off < len(content); off += 8000 {
		res := entry.Handler(context.Background(), map[string]interface{}{
			"id": id, "offset": float64(off), "limit": float64(8000),
		})
		if !res.Success {
			t.Fatalf("read_result at %d failed: %s", off, res.Error)
		}
		body := res.Output
		if i := strings.Index(body, "]\n\n"); i >= 0 {
			body = body[i+3:]
		}
		got.WriteString(body)
	}
	if !strings.Contains(got.String(), needle) {
		t.Error("the omitted region was not retrievable through read_result — " +
			"the model would have no way to reach it")
	}
}

// TestReadResultIsReadOnly — it must be offered in Chat mode, where mutating
// tools are refused, or a read-only turn loses access to its own tool output.
func TestReadResultIsReadOnly(t *testing.T) {
	r, _ := registryWithSpill(t)
	entry, ok := r.Get("read_result")
	if !ok {
		t.Fatal("read_result not registered")
	}
	if !entry.ReadOnly {
		t.Error("read_result is not marked read-only, so Chat mode will refuse it")
	}
}

// TestObserveResultWithoutAStoreStillReduces — the registry must not depend on
// spilling being available.
func TestObserveResultWithoutAStoreStillReduces(t *testing.T) {
	r := NewRegistry()
	content := strings.Repeat("x", 50_000)
	obs := r.ObserveResult("t", content)
	if len(obs) >= len(content) {
		t.Error("a registry with no spill store did not reduce a huge result")
	}
}

func TestRegisterSpillToolIsNilSafe(t *testing.T) {
	RegisterSpillTool(nil, nil)
	r := NewRegistry()
	RegisterSpillTool(r, nil)
	if _, ok := r.Get("read_result"); ok {
		t.Error("read_result was registered without a store, so every call would fail")
	}
}

func TestReadResultRejectsMissingID(t *testing.T) {
	r, _ := registryWithSpill(t)
	entry, _ := r.Get("read_result")
	if res := entry.Handler(context.Background(), map[string]interface{}{}); res.Success {
		t.Error("read_result accepted a call with no id")
	}
}

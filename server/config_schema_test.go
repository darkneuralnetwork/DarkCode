package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkcode/config"
)

// TestConfigSchemaDescribesEveryTier. An interface renders the default view
// from primary and hides the rest, so all three tiers have to arrive.
func TestConfigSchemaDescribesEveryTier(t *testing.T) {
	s := newTestServer(&config.Config{Model: "gpt-4o"})
	w := httptest.NewRecorder()
	s.handleConfigSchema(w, httptest.NewRequest("GET", "/api/config/schema", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Fields       []config.Field `json:"fields"`
		Groups       []string       `json:"groups"`
		Values       map[string]any `json:"values"`
		PrimaryCount int            `json:"primary_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tiers := map[config.Tier]int{}
	for _, f := range body.Fields {
		tiers[f.Tier]++
	}
	for _, want := range []config.Tier{config.TierPrimary, config.TierAdvanced, config.TierDerived} {
		if tiers[want] == 0 {
			t.Errorf("no %s fields in the schema", want)
		}
	}
	if body.PrimaryCount != tiers[config.TierPrimary] {
		t.Errorf("primary_count = %d but %d primary fields were sent", body.PrimaryCount, tiers[config.TierPrimary])
	}
	if len(body.Groups) == 0 {
		t.Error("no groups, so an interface cannot lay the fields out")
	}
	if body.Values["model"] != "gpt-4o" {
		t.Errorf("values did not carry the live config: %v", body.Values["model"])
	}
}

// TestConfigSchemaNeverLeaksASecret. The schema is served to the browser and
// carries live values, so this is the endpoint where a redaction miss would
// actually escape.
func TestConfigSchemaNeverLeaksASecret(t *testing.T) {
	const key = "sk-live-must-not-appear"
	s := newTestServer(&config.Config{Model: "gpt-4o", APIKey: key})
	w := httptest.NewRecorder()
	s.handleConfigSchema(w, httptest.NewRequest("GET", "/api/config/schema", nil))

	if strings.Contains(w.Body.String(), key) {
		t.Error("the config schema response contains the API key verbatim")
	}
}

func TestConfigSchemaRejectsNonGET(t *testing.T) {
	s := newTestServer(&config.Config{})
	w := httptest.NewRecorder()
	s.handleConfigSchema(w, httptest.NewRequest("POST", "/api/config/schema", nil))
	if w.Code != 405 {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

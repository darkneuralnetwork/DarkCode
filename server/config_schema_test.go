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

// TestBackgroundWorkIsSettableOverTheAPI. It is a primary setting, and a
// primary setting the API will not accept is one the browser cannot offer —
// exactly the gap the descriptors exist to close.
func TestBackgroundWorkIsSettableOverTheAPI(t *testing.T) {
	if !config.Described("background_work") {
		t.Fatal("background_work has no descriptor")
	}
	var primary bool
	for _, f := range config.FieldsInTier(config.TierPrimary) {
		if f.Name == "background_work" {
			primary = true
		}
	}
	if !primary {
		t.Error("background_work is not offered as a primary setting")
	}

	s := newTestServer(&config.Config{Model: "gpt-4o"})
	for _, level := range []string{"off", "light", "full"} {
		w := httptest.NewRecorder()
		s.updateConfig(w, httptest.NewRequest("POST", "/api/config",
			strings.NewReader(`{"action":"update_settings","background_work":"`+level+`"}`)))
		if w.Code != 200 {
			t.Errorf("setting background_work=%q: status %d, body %s", level, w.Code, w.Body.String())
			continue
		}
		if got := s.cfg.ResolvedBackgroundWork(); got != level {
			t.Errorf("background_work = %q after setting %q", got, level)
		}
	}

	w := httptest.NewRecorder()
	s.updateConfig(w, httptest.NewRequest("POST", "/api/config",
		strings.NewReader(`{"action":"update_settings","background_work":"nonsense"}`)))
	if w.Code != 400 {
		t.Errorf("an invalid level returned %d, want 400", w.Code)
	}
}

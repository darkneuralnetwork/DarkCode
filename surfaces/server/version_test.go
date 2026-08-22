package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
)

// Every surface that names this build must report the same thing, and that
// thing must come from the build stamp.
//
// These all used to be the literal "1.0.0", three minor versions behind, while
// build.sh stamped a variable that did not exist. /api/health is what
// deployment tooling reads to find out what is running, so it was answering
// the one question it exists to answer with a wrong constant.
func TestHealthReportsTheBuildVersion(t *testing.T) {
	orig := core.Version
	core.Version = "9.9.9-test"
	defer func() { core.Version = orig }()

	s := &Server{}
	w := httptest.NewRecorder()
	s.handleHealth(w, httptest.NewRequest("GET", "/api/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != "9.9.9-test" {
		t.Errorf("health reports version %v, want the build stamp", got["version"])
	}
}

// An unstamped build is legitimate — plain `go build` and `go test` produce one
// — and it should say so rather than render as an empty string.
func TestUnstampedBuildSaysDev(t *testing.T) {
	orig := core.Version
	core.Version = ""
	defer func() { core.Version = orig }()
	if got := core.VersionOrDev(); got != "dev" {
		t.Errorf("VersionOrDev() = %q, want %q", got, "dev")
	}
}

// The product version and the protocol versions are different things. Folding
// one into the other would break clients negotiating on protocol.
func TestProtocolVersionsAreNotTheProductVersion(t *testing.T) {
	orig := core.Version
	core.Version = "9.9.9-test"
	defer func() { core.Version = orig }()

	if HTPVersion == core.Version {
		t.Error("the HTP protocol version tracks the product version; they are unrelated")
	}
	if !strings.HasPrefix(HTPVersion, "1.") {
		t.Errorf("HTPVersion = %q, which no longer looks like a protocol version", HTPVersion)
	}
}

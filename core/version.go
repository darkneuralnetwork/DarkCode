package core

// version.go — what build this is.
//
// The value is stamped at link time by build.sh:
//
//	-X github.com/darkcode/core.Version=1.3.2
//
// It lives here, in the package everything already depends on, because the
// alternative is threading a string through every constructor that needs to
// name itself — the server, the MCP handshake, the HTP envelope — none of which
// have any other reason to know about each other.
//
// This replaced a stamp aimed at `main.Version`, a variable that was never
// declared. The linker does not object to -X naming a symbol that does not
// exist; it simply writes nothing and exits zero. So the build reported success,
// every release went out believing it was stamped, and seven call sites went on
// reporting a hardcoded "1.0.0" — including /api/health, which is exactly what
// deployment tooling reads to find out what is running.
//
// build.sh now asserts the built binary reports the version it was asked for,
// so a stamp that silently does nothing fails the build instead of shipping.
var Version = "dev"

// VersionOrDev reports the build version, never the empty string.
//
// An unstamped build is a legitimate state — `go build ./...` and `go test`
// produce one — and it should say so plainly rather than render as a blank.
func VersionOrDev() string {
	if Version == "" {
		return "dev"
	}
	return Version
}

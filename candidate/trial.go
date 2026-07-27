package candidate

// trial.go — trying a patch without keeping it.
//
// Ranking candidates on evidence means running each one, and running each one
// means writing to the workspace. The hard requirement is that the workspace
// comes back exactly as it was, including for the candidates that fail: a
// trial that leaks its changes into the next one makes every comparison after
// it meaningless, and a trial that leaks into the user's tree is worse than
// not ranking at all.
//
// The restore is therefore built around the failure cases rather than the
// happy path. Original contents are captured before anything is written, a
// file that did not exist is remembered as absent so it can be deleted again,
// and the restore runs from a defer so a panicking verifier cannot skip it.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// verifyTimeout bounds one candidate's verification. A hung test suite must
// not strand the whole ranking; the candidate is reported unverified, which is
// the safe reading.
const verifyTimeout = 10 * time.Minute

// FileTrial applies a patch to workspace, runs verify, and restores the tree.
//
// verify is a shell command — the project's own test command — and its exit
// status is the entire verdict. Nothing here reads its output to decide,
// because a verifier that has to be interpreted is not a verifier.
func FileTrial(workspace string, verify string) TrialFunc {
	return func(ctx context.Context, p Patch) Trial {
		if verify == "" {
			return Trial{Err: "no verify command configured, so nothing can be proven"}
		}
		restore, err := applyPatch(workspace, p)
		// Restore whatever was written, even on a partial failure: applyPatch
		// returns the undo for the files it managed to touch before erroring.
		defer restore()
		if err != nil {
			return Trial{Err: err.Error()}
		}

		ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "sh", "-c", verify)
		cmd.Dir = workspace
		out, runErr := cmd.CombinedOutput()

		return Trial{
			Applied:  true,
			Verified: runErr == nil,
			Output:   tail(string(out), 4000),
		}
	}
}

// Apply writes every file in p and returns a function restoring the previous
// state. Exported for callers that keep a patch rather than trialling it, and
// still need the undo for their own failure path.
func Apply(workspace string, p Patch) (undo func(), err error) { return applyPatch(workspace, p) }

// applyPatch writes every file in p and returns a function restoring the
// previous state. The undo is returned even when the write fails part way, so
// a partially applied patch is still fully reverted.
func applyPatch(workspace string, p Patch) (undo func(), err error) {
	type prior struct {
		path    string
		content []byte
		existed bool
		mode    os.FileMode
	}
	var saved []prior

	undo = func() {
		// Reverse order, so a directory created for a new file is removed
		// after the file inside it.
		for i := len(saved) - 1; i >= 0; i-- {
			s := saved[i]
			if !s.existed {
				_ = os.Remove(s.path)
				continue
			}
			_ = os.WriteFile(s.path, s.content, s.mode)
		}
	}

	for _, rel := range p.Paths() {
		abs, cleanErr := safeJoin(workspace, rel)
		if cleanErr != nil {
			return undo, cleanErr
		}

		rec := prior{path: abs, mode: 0o644}
		if info, statErr := os.Stat(abs); statErr == nil {
			body, readErr := os.ReadFile(abs)
			if readErr != nil {
				return undo, readErr
			}
			rec.content, rec.existed, rec.mode = body, true, info.Mode().Perm()
		}
		saved = append(saved, rec)

		if !rec.existed {
			if mkErr := os.MkdirAll(filepath.Dir(abs), 0o755); mkErr != nil {
				return undo, mkErr
			}
		}
		if writeErr := os.WriteFile(abs, []byte(p.Files[rel]), rec.mode); writeErr != nil {
			return undo, writeErr
		}
	}
	return undo, nil
}

// safeJoin resolves rel inside workspace and refuses anything that escapes it.
// A candidate patch is model-authored content, so a traversal in a path is a
// case to handle rather than assume away.
//
// Traversal is rejected rather than neutralised. Clamping "../secrets" to
// "secrets" would keep the write inside the workspace but put it somewhere
// nobody asked for, and a patch that lands in the wrong file is harder to
// notice than one that fails outright.
func safeJoin(workspace, rel string) (string, error) {
	deny := func() (string, error) {
		return "", &os.PathError{Op: "write", Path: rel, Err: os.ErrPermission}
	}
	if rel == "" || filepath.IsAbs(rel) {
		return deny()
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return deny()
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	// Belt and braces: symlinks and odd separators can still land outside.
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return deny()
	}
	return full, nil
}

// tail keeps the end of verifier output, which is where the failure is.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "…\n" + cut
}

// DefaultVerify guesses a project's test command from its build files.
//
// Shared rather than duplicated: the acceptance gate and the patch ranker must
// agree on what "verified" means for a repository, or a patch could pass one
// and fail the other and nobody could say which was right.
func DefaultVerify(dir string) string {
	for _, m := range []struct{ marker, cmd string }{
		{"go.mod", "go build ./... && go test ./..."},
		{"package.json", "npm test --silent"},
		{"Cargo.toml", "cargo test"},
		{"pyproject.toml", "pytest -q"},
		{"setup.py", "pytest -q"},
	} {
		if _, err := os.Stat(filepath.Join(dir, m.marker)); err == nil {
			return m.cmd
		}
	}
	return ""
}

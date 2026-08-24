package observability

// recover.go — keeping a background failure from becoming a process failure.
//
// Work that runs on its own goroutine and is not essential to the session —
// indexing, warming a cache, syncing the graph — has a property that is easy to
// miss: an unrecovered panic there does not fail that work, it ends the whole
// program. There is no caller to return an error to and no frame in between to
// stop it, so a single surprise in a file being parsed takes down an agent that
// was otherwise fine.
//
// That is exactly how a method on a generic type became a fatal startup bug:
// the receiver was an AST shape the parser did not expect, and because the
// index runs in the background, opening the repository killed the process.
//
// Guard is for the degradable kind of work only. Anything whose failure should
// stop the program should be allowed to panic.

import (
	"fmt"
	"runtime/debug"
)

// Guard runs fn and turns a panic into a logged error.
//
// name identifies the work in the log, since the goroutine that dies is rarely
// the one a reader is looking at. The stack is kept: a background panic with no
// stack is nearly impossible to place afterwards.
func Guard(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			Log().Error("background task panicked", fmt.Errorf("%v", r), map[string]interface{}{
				"task":  name,
				"stack": string(debug.Stack()),
			})
		}
	}()
	fn()
}

// Go starts fn on its own goroutine under Guard.
func Go(name string, fn func()) {
	go Guard(name, fn)
}

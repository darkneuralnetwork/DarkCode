package core

import "strings"

// LogSafe strips line terminators from a value before it is interpolated into
// a line-oriented log.
//
// The structured logger in package observability is immune to this: it
// json.Marshals each entry, which escapes newlines. Twenty-one sites still use
// stdlib log.Printf, and three of them interpolate values the model controls —
// a tool name, a project id, an error carrying a model-supplied path. CodeQL
// found those three as go/log-injection.
//
// A forged line matters here more than in most programs, because this log is
// the record of what the agent did to the user's machine. A tool named
//
//	foo\n2026/08/15 04:00:00 [permission] user approved rm -rf /
//
// writes a second line that reads exactly like a real one. Replacing the
// terminators with an escape keeps the value legible while making it
// impossible for it to end its own line.
// The replacer is package-level and every value goes through it. An earlier
// version returned the input unchanged when it contained no terminator, which
// was a branch on which the untrusted value reached the output having passed
// through nothing — CodeQL was right to keep reporting it, and the saving was
// never worth having on a logging path.
var logLineBreaks = strings.NewReplacer("\r\n", "\\n", "\n", "\\n", "\r", "\\r")

func LogSafe(s string) string { return logLineBreaks.Replace(s) }

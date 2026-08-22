package intelligence

import "testing"

// symbolSet indexes a parse result by symbol name → kind.
func symbolSet(r ParseResult) map[string]string {
	out := map[string]string{}
	for _, s := range r.Symbols {
		out[s.Name] = s.Kind
	}
	return out
}

func importSet(r ParseResult) map[string]bool {
	out := map[string]bool{}
	for _, i := range r.Imports {
		out[i.Path] = true
	}
	return out
}

func hasEmbed(r ParseResult, child, parent string) bool {
	for _, e := range r.Embeds {
		if e.Type == child && e.Embedded == parent {
			return true
		}
	}
	return false
}

func hasCall(r ParseResult, caller, callee string) bool {
	for _, c := range r.Calls {
		if c.Caller == caller && c.Callee == callee {
			return true
		}
	}
	return false
}

func TestLanguageOf(t *testing.T) {
	cases := map[string]string{
		"a/b/main.go": "go", "x.ts": "typescript", "x.tsx": "typescript",
		"x.js": "typescript", "x.mjs": "typescript", "s.py": "python",
		"lib.rs": "rust", "App.java": "java", "notes.md": "", "Makefile": "",
	}
	for path, want := range cases {
		if got := LanguageOf(path); got != want {
			t.Errorf("LanguageOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestParseTypeScript(t *testing.T) {
	src := `
import { Logger } from "./logger";
const fs = require("fs");

export interface Shape { area(): number; }

export abstract class Base {}

export class Circle extends Base implements Shape {
  area(): number {
    return computeArea(this.r);
  }
}

export function main() {
  const c = new Circle();
  render(c);
}

export const helper = async (x: number) => doWork(x);
`
	r := ParseText([]byte(src), "app.ts")
	syms := symbolSet(r)
	for name, kind := range map[string]string{
		"Shape": "interface", "Base": "class", "Circle": "class",
		"main": "function", "area": "method", "helper": "function",
	} {
		if syms[name] != kind {
			t.Errorf("symbol %s = %q, want %q", name, syms[name], kind)
		}
	}

	imports := importSet(r)
	if !imports["./logger"] || !imports["fs"] {
		t.Errorf("imports = %v, want ./logger and fs", imports)
	}
	if !hasEmbed(r, "Circle", "Base") {
		t.Error("missing extends edge Circle → Base")
	}
	if !hasEmbed(r, "Circle", "Shape") {
		t.Error("missing implements edge Circle → Shape")
	}
	if !hasCall(r, "main", "render") {
		t.Errorf("missing call edge main → render; calls = %+v", r.Calls)
	}
}

func TestParsePython(t *testing.T) {
	src := `
import os
from typing import List

class Animal:
    pass

class Dog(Animal):
    def speak(self):
        return make_sound("woof")

def main():
    d = Dog()
    d.speak()
`
	r := ParseText([]byte(src), "zoo.py")
	syms := symbolSet(r)
	for name, kind := range map[string]string{
		"Animal": "class", "Dog": "class", "speak": "function", "main": "function",
	} {
		if syms[name] != kind {
			t.Errorf("symbol %s = %q, want %q", name, syms[name], kind)
		}
	}
	imports := importSet(r)
	if !imports["os"] || !imports["typing"] {
		t.Errorf("imports = %v, want os and typing", imports)
	}
	if !hasEmbed(r, "Dog", "Animal") {
		t.Error("missing inheritance edge Dog → Animal")
	}
	if !hasCall(r, "speak", "make_sound") {
		t.Errorf("missing call edge speak → make_sound; calls = %+v", r.Calls)
	}
}

func TestParseRust(t *testing.T) {
	src := `
use std::collections::HashMap;
pub use crate::config;

pub struct Server { port: u16 }

pub trait Handler { fn handle(&self); }

impl Handler for Server {
    fn handle(&self) {
        log_request();
    }
}

pub async fn run() {
    let s = Server { port: 80 };
    start(s);
}
`
	r := ParseText([]byte(src), "main.rs")
	syms := symbolSet(r)
	for name, kind := range map[string]string{
		"Server": "struct", "Handler": "interface", "handle": "function", "run": "function",
	} {
		if syms[name] != kind {
			t.Errorf("symbol %s = %q, want %q", name, syms[name], kind)
		}
	}
	if !importSet(r)["std::collections::HashMap"] {
		t.Errorf("imports = %v, want the std path", importSet(r))
	}
	// `impl Handler for Server` means Server implements Handler.
	if !hasEmbed(r, "Handler", "Server") {
		t.Errorf("missing impl edge; embeds = %+v", r.Embeds)
	}
	if !hasCall(r, "run", "start") {
		t.Errorf("missing call edge run → start; calls = %+v", r.Calls)
	}
}

func TestParseJava(t *testing.T) {
	src := `
package com.example;

import java.util.List;
import static java.lang.Math.max;

public interface Greeter { String greet(); }

public abstract class Base {}

public class Hello extends Base implements Greeter {
    public String greet() {
        return format("hi");
    }
}
`
	r := ParseText([]byte(src), "Hello.java")
	syms := symbolSet(r)
	for name, kind := range map[string]string{
		"Greeter": "interface", "Base": "class", "Hello": "class", "greet": "method",
	} {
		if syms[name] != kind {
			t.Errorf("symbol %s = %q, want %q", name, syms[name], kind)
		}
	}
	imports := importSet(r)
	if !imports["java.util.List"] || !imports["java.lang.Math.max"] {
		t.Errorf("imports = %v", imports)
	}
	if !hasEmbed(r, "Hello", "Base") {
		t.Error("missing extends edge Hello → Base")
	}
	if !hasEmbed(r, "Hello", "Greeter") {
		t.Error("missing implements edge Hello → Greeter")
	}
}

// Declarations quoted inside strings or commented out must not become symbols;
// this is what the noise-stripping pass buys.
func TestParseIgnoresCommentsAndStrings(t *testing.T) {
	src := `
// export class CommentedClass {}
/* export function BlockCommented() {} */
const sample = "export class StringClass {}";
const tmpl = ` + "`export function TemplateFn() {}`" + `;
export class RealClass {}
`
	syms := symbolSet(ParseText([]byte(src), "a.ts"))
	for _, ghost := range []string{"CommentedClass", "BlockCommented", "StringClass", "TemplateFn"} {
		if _, found := syms[ghost]; found {
			t.Errorf("%s was extracted from a comment or string literal", ghost)
		}
	}
	if syms["RealClass"] != "class" {
		t.Errorf("real declaration missing: %v", syms)
	}
}

func TestParseReportsAccurateLineNumbers(t *testing.T) {
	src := "// line 1\n\nclass Second {}\n\nfunction Fourth() {}\n"
	r := ParseText([]byte(src), "a.ts")
	want := map[string]int{"Second": 3, "Fourth": 5}
	for _, s := range r.Symbols {
		if exp, ok := want[s.Name]; ok && s.Line != exp {
			t.Errorf("%s on line %d, want %d", s.Name, s.Line, exp)
		}
	}
}

func TestParseUnsupportedExtensionIsEmpty(t *testing.T) {
	r := ParseText([]byte("anything at all"), "notes.md")
	if len(r.Symbols) != 0 || len(r.Imports) != 0 || len(r.Calls) != 0 {
		t.Errorf("unsupported file produced %+v", r)
	}
}

// Control-flow keywords are followed by parentheses but are not calls.
func TestParseSkipsControlFlowKeywords(t *testing.T) {
	src := `
function run() {
  if (ready()) {
    for (let i = 0; i < 3; i++) { work(); }
  }
  while (waiting()) {}
  switch (mode()) {}
}
`
	r := ParseText([]byte(src), "a.js")
	for _, c := range r.Calls {
		if notCalls[c.Callee] {
			t.Errorf("keyword %q was recorded as a call", c.Callee)
		}
	}
	for _, want := range []string{"ready", "work", "waiting", "mode"} {
		if !hasCall(r, "run", want) {
			t.Errorf("missing call edge run → %s", want)
		}
	}
}

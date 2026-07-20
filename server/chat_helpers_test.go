package server

import (
	"strings"
	"testing"
)

func TestExtractJSONObjectToleratesFencesAndProse(t *testing.T) {
	cases := []struct{ in, wantSub string }{
		{"```json\n{\"mode\":\"loop\",\"is_new_project\":true}\n```", `"mode":"loop"`},
		{"Here you go: {\"mode\":\"project\"} — done.", `"mode":"project"`},
		{`{"mode":"general"}`, `"mode":"general"`},
	}
	for _, c := range cases {
		got := extractJSONObject(c.in)
		if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") || !strings.Contains(got, c.wantSub) {
			t.Errorf("extractJSONObject(%q) = %q", c.in, got)
		}
	}
	// No braces: returned unchanged so Unmarshal reports the real error.
	if got := extractJSONObject("no json here"); got != "no json here" {
		t.Errorf("no-brace input mangled: %q", got)
	}
}

func TestIsSkeletonBlueprint(t *testing.T) {
	if !isSkeletonBlueprint("") || !isSkeletonBlueprint("  \n") {
		t.Error("empty content should count as skeleton")
	}
	if !isSkeletonBlueprint("# Implementation Plan\n\n_Status: awaiting first task_\n") {
		t.Error("seed skeleton should count as skeleton")
	}
	if isSkeletonBlueprint("# Implementation Plan\n\n## Goal Description\nreal plan content") {
		t.Error("a real plan must never be treated as skeleton")
	}
}

func TestIsBuildIntent(t *testing.T) {
	cases := []struct {
		query, mode string
		want        bool
	}{
		{"create a python flask website for blog posting", "project", true},
		{"anything at all", "loop", true},
		{"please build me a REST API", "project", true},
		{"set up a new react app", "project", true},
		{"what does the auth middleware do?", "project", false},
		{"explain how JWT works", "general", false},
		{"read the config file", "project", false},
	}
	for _, c := range cases {
		if got := isBuildIntent(c.query, c.mode); got != c.want {
			t.Errorf("isBuildIntent(%q, %s) = %v, want %v", c.query, c.mode, got, c.want)
		}
	}
}

func TestDeriveProjectName(t *testing.T) {
	if n := deriveProjectName("create a small static website for a coffee shop"); n != "small static website for a" {
		t.Errorf("name = %q", n)
	}
	if n := deriveProjectName(""); n != "Untitled Build" {
		t.Errorf("empty query name = %q", n)
	}
}

func TestLooksLikeBuild(t *testing.T) {
	build := []string{
		"build a simple todo list web page with add and delete",
		"create a python flask website for blog posting",
		"make me a CLI tool for renaming files",
		"write a script that scrapes a site",
	}
	for _, q := range build {
		if !looksLikeBuild(q) {
			t.Errorf("looksLikeBuild(%q) = false, want true", q)
		}
	}
	notBuild := []string{
		"what is the capital of France",
		"explain how flask handles routing",
		"summarize this file",
		"build up my confidence", // verb but no artifact noun
	}
	for _, q := range notBuild {
		if looksLikeBuild(q) {
			t.Errorf("looksLikeBuild(%q) = true, want false", q)
		}
	}
}

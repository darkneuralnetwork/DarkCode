package server

import "testing"

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

func TestDeriveProjectName(t *testing.T) {
	if n := deriveProjectName("create a small static website for a coffee shop"); n != "small static website for a" {
		t.Errorf("name = %q", n)
	}
	if n := deriveProjectName(""); n != "Untitled Build" {
		t.Errorf("empty query name = %q", n)
	}
}

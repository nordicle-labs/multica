package main

import "testing"

func TestTemplate(t *testing.T) {
	if DefaultBranch != "main" {
		t.Fatal("unexpected default branch")
	}
}

const DefaultBranch = "main"

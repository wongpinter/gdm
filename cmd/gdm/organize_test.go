package main

import (
	"strings"
	"testing"
)

func TestRunOrganizeHelp(t *testing.T) {
	var out strings.Builder
	if err := runOrganize([]string{"tv", "--help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Usage: gdm organize") {
		t.Fatalf("help output = %q", out.String())
	}
}

func TestRunOrganizeRequiresPaths(t *testing.T) {
	var out strings.Builder
	if err := runOrganize([]string{"movie"}, &out); err == nil {
		t.Fatal("organize accepted missing input/output")
	}
}

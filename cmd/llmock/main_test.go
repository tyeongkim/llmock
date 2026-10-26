package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	reply := filepath.Join(dir, "reply.txt")
	if err := os.WriteFile(reply, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"root help", []string{"--help"}, 0},
		{"help subcommand", []string{"help", "serve"}, 0},
		{"serve help", []string{"serve", "--help"}, 0},
		{"validate short help", []string{"validate", "-h"}, 0},
		{"version", []string{"--version"}, 0},
		{"unknown command", []string{"bogus"}, 2},
		{"unknown flag", []string{"serve", "--nope"}, 2},
		{"missing config", []string{"validate"}, 2},
		{"config and txt together", []string{"validate", "-c", reply, reply}, 2},
		{"missing file", []string{"validate", filepath.Join(dir, "absent.txt")}, 1},
		{"valid txt", []string{"validate", reply}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Fatalf("run(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

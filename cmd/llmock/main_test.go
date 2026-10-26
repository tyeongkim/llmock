package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tyeongkim/llmock/internal/config"
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
		{"record open failure", []string{"serve", "--record", filepath.Join(dir, "missing", "requests.jsonl"), reply}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Fatalf("run(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestEmbeddedExampleIsValid(t *testing.T) {
	dir := t.TempDir()
	entries, err := exampleFiles.ReadDir("examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := exampleFiles.ReadFile("examples/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := config.Load(filepath.Join(dir, "llmock.yaml")); err != nil {
		t.Fatalf("embedded example is not loadable: %v", err)
	}
}

func TestHelpTopics(t *testing.T) {
	for _, topic := range []string{"config", "routes", "record", "errors", "example", "version", "help"} {
		t.Run(topic, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "help")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			stdout := os.Stdout
			os.Stdout = output
			defer func() { os.Stdout = stdout }()
			if code := run([]string{"help", topic}); code != 0 {
				t.Fatalf("help %s exited %d", topic, code)
			}
			info, err := output.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() == 0 {
				t.Fatalf("help %s produced no output", topic)
			}
		})
	}
	if code := run([]string{"help", "unknown-topic"}); code != 2 {
		t.Fatalf("unknown help topic exited %d, want 2", code)
	}
}

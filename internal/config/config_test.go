package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadResolvesFilesRelativeToConfig(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"llmock.yaml": `
stream: { chunk_delay: 20ms }
responses:
  - file: hello.txt
    delay: 1s
  - status: 503
  - tool_call: call.json
`,
		"hello.txt": "hi there\n",
		"call.json": `{"name":"get_weather","arguments":{"city": "Seoul"}}`,
	})

	cfg, err := Load(filepath.Join(dir, "llmock.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Stream.ChunkDelay != 20*time.Millisecond {
		t.Fatalf("chunk_delay = %v", cfg.Stream.ChunkDelay)
	}
	if got := cfg.Responses[0]; got.Content != "hi there\n" || got.Delay != time.Second {
		t.Fatalf("responses[0] = %+v", got)
	}
	if cfg.Responses[1].Status != 503 {
		t.Fatalf("responses[1] = %+v", cfg.Responses[1])
	}
	if calls := cfg.Responses[2].ToolCalls; len(calls) != 1 || calls[0].Arguments != `{"city":"Seoul"}` {
		t.Fatalf("responses[2].tool_calls = %+v", calls)
	}
}

func TestLoadRejectsInvalidResponses(t *testing.T) {
	cases := map[string]string{
		"two kinds":      "responses:\n  - { file: a.txt, status: 500 }\n",
		"no kind":        "responses:\n  - { delay: 1s }\n",
		"non-error code": "responses:\n  - { status: 200 }\n",
		"empty":          "responses: []\n",
		"bad mode":       "mode: random\nresponses:\n  - { status: 500 }\n",
		"missing file":   "responses:\n  - { file: missing.txt }\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"llmock.yaml": yaml, "a.txt": "a"})
			if _, err := Load(filepath.Join(dir, "llmock.yaml")); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

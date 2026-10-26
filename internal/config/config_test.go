package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestNewConfigValidation(t *testing.T) {
	cases := []struct{ name, yaml, location string }{
		{"empty-name", `routes: [{name: "", match: {model: gpt}, responses: [{status: 500}]}]`, "routes[0]"},
		{"duplicate-name", `routes: [{name: a, match: {model: gpt}, responses: [{status: 500}]}, {name: a, match: {model: gpt}, responses: [{status: 500}]}]`, "routes[1]"},
		{"empty-match", `routes: [{name: a, match: {}, responses: [{status: 500}]}]`, "routes[0].match"},
		{"empty-route-responses", `routes: [{name: a, match: {model: gpt}, responses: []}]`, "routes[0]"},
		{"route-status", `routes: [{name: a, match: {model: gpt}, responses: [{status: 200}]}]`, "routes[0].responses[0]"},
		{"route-policy", `routes: [{name: a, match: {model: gpt}, on_exhausted: bogus, responses: [{status: 500}]}]`, "routes[0]"},
		{"zero-abort", `responses: [{file: a.txt, abort_after_chunks: 0}]`, "responses[0]"},
		{"negative-abort", `responses: [{file: a.txt, abort_after_chunks: -1}]`, "responses[0]"},
		{"status-abort", `responses: [{status: 429, abort_after_chunks: 1}]`, "responses[0]"},
		{"status-stream", `responses: [{status: 429, stream: {chunk_size: 1}}]`, "responses[0]"},
		{"zero-response-chunk-size", `responses: [{file: a.txt, stream: {chunk_size: 0}}]`, "responses[0]"},
		{"negative-response-chunk-delay", `responses: [{file: a.txt, stream: {chunk_delay: -1ms}}]`, "responses[0]"},
		{"route-status-tool", `routes: [{name: a, match: {model: gpt}, responses: [{status: 500, tool_call: call.json}]}]`, "routes[0].responses[0]"},
		{"zero-global-chunk-size", `stream: {chunk_size: 0}
responses: [{file: a.txt}]`, "stream.chunk_size"},
		{"negative-global-chunk-size", `stream: {chunk_size: -1}
responses: [{file: a.txt}]`, "stream.chunk_size"},
		{"negative-global-chunk-delay", `stream: {chunk_delay: -1ms}
responses: [{file: a.txt}]`, "stream.chunk_delay"},
		{"negative-delay", `responses: [{file: a.txt, delay: -1ms}]`, "responses[0]"},
		{"negative-prompt", `responses: [{file: a.txt, usage: {prompt_tokens: -1}}]`, "responses[0]"},
		{"negative-completion", `responses: [{file: a.txt, usage: {completion_tokens: -1}}]`, "responses[0]"},
		{"negative-total", `responses: [{file: a.txt, usage: {total_tokens: -1}}]`, "responses[0]"},
		{"empty-system-contains", `routes: [{name: a, match: {system_contains: ""}, responses: [{status: 500}]}]`, "routes[0].match"},
		{"empty-last-user-contains", `routes: [{name: a, match: {last_user_contains: ""}, responses: [{status: 500}]}]`, "routes[0].match"},
		{"empty-header", `responses: [{status: 429, headers: {"": value}}]`, "responses[0]"},
		{"invalid-header", `responses: [{status: 429, headers: {"bad name": value}}]`, "responses[0]"},
		{"header-newline", `responses: [{status: 429, headers: {X-Mock: "value\nInjected: yes"}}]`, "responses[0]"},
		{"header-carriage-return", `responses: [{status: 429, headers: {X-Mock: "value\rInjected: yes"}}]`, "responses[0]"},
		{"reserved-content-type", `responses: [{status: 429, headers: {content-type: value}}]`, "responses[0]"},
		{"reserved-content-length", `responses: [{status: 429, headers: {Content-Length: value}}]`, "responses[0]"},
		{"reserved-transfer-encoding", `responses: [{status: 429, headers: {Transfer-Encoding: value}}]`, "responses[0]"},
		{"reserved-connection", `responses: [{status: 429, headers: {Connection: value}}]`, "responses[0]"},
		{"unknown-response-field", `responses: [{file: a.txt, abort_after_chunk: 2}]`, "abort_after_chunk"},
		{"unknown-match-field", `routes: [{name: a, match: {has_tool: true}, responses: [{status: 500}]}]`, "has_tool"},
		{"unknown-top-level-field", `on_exhaused: loop
responses: [{status: 500}]`, "on_exhaused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"llmock.yaml": tc.yaml, "a.txt": "a"})
			_, err := Load(filepath.Join(dir, "llmock.yaml"))
			if err == nil || !strings.Contains(err.Error(), tc.location) {
				t.Fatalf("err=%v, want location %s", err, tc.location)
			}
		})
	}
}

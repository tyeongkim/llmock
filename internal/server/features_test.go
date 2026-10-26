package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyeongkim/llmock/internal/config"
	"github.com/tyeongkim/llmock/internal/server"
)

func postJSON(t *testing.T, baseURL, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST completion %s: %v", body, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read completion %s: %v", body, err)
	}
	return resp.StatusCode, string(raw)
}

func loadConfig(t *testing.T, text string) config.Config {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"llmock.yaml": text, "reply.txt": "hello", "call.json": `{"name":"lookup","arguments":{}}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(filepath.Join(dir, "llmock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testServer(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.New(cfg, nil).Handler())
	t.Cleanup(ts.Close)
	return ts
}

type loggedRequest struct {
	Index         int             `json:"index"`
	ReceivedAt    string          `json:"received_at"`
	Route         *string         `json:"route"`
	ResponseIndex *int            `json:"response_index"`
	Exhausted     bool            `json:"exhausted"`
	Status        *int            `json:"status"`
	Body          json.RawMessage `json:"body"`
}

type recordBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *recordBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func recordingServer(t *testing.T, cfg config.Config) (*httptest.Server, *recordBuffer) {
	t.Helper()
	recorder := &recordBuffer{}
	ts := httptest.NewServer(server.New(cfg, recorder).Handler())
	t.Cleanup(ts.Close)
	return ts, recorder
}

func recordedRequests(t *testing.T, recorder *recordBuffer) []loggedRequest {
	t.Helper()
	recorder.mu.Lock()
	text := recorder.buffer.String()
	recorder.mu.Unlock()
	var records []loggedRequest
	if text == "" {
		return records
	}
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("incomplete JSONL record: %q", text)
	}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		var record loggedRequest
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("JSONL line %s: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestLoadedDefaultExhaustion(t *testing.T) {
	ts := testServer(t, loadConfig(t, "responses: [{file: reply.txt, tool_call: call.json}]"))
	status, body := postJSON(t, ts.URL, `{}`)
	if status != 200 || !strings.Contains(body, `"content":"hello"`) || !strings.Contains(body, `"tool_calls"`) {
		t.Fatalf("first request: %d %s", status, body)
	}
	status, body = postJSON(t, ts.URL, `{}`)
	if status != 500 || !strings.Contains(body, "mock_responses_exhausted") {
		t.Fatalf("exhausted request: %d %s", status, body)
	}
}

func TestTextFileRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.txt")
	if err := os.WriteFile(path, []byte("again"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromTextFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := testServer(t, cfg)
	for i := 0; i < 3; i++ {
		status, body := postJSON(t, ts.URL, `{}`)
		if status != 200 || !strings.Contains(body, `"content":"again"`) {
			t.Fatalf("request %d: %d %s", i+1, status, body)
		}
	}
}

func TestRecordingBeforeResponse(t *testing.T) {
	ts, recorder := recordingServer(t, loadConfig(t, "responses: [{file: reply.txt}, {status: 429}]"))
	bodies := []string{`{"model":"test","extra":{"x":1},"stream":true}`, `{}`, `{}`, `broken`}
	expectedBodies := []string{bodies[0], bodies[1], bodies[2], `"broken"`}
	expectedStatuses := []int{200, 429, 500, 400}
	for i, body := range bodies {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("request %s: %v", body, err)
		}
		records := recordedRequests(t, recorder)
		resp.Body.Close()
		if len(records) != i+1 {
			t.Fatalf("request %s: %d lines at response headers, want %d", body, len(records), i+1)
		}
		record := records[i]
		if record.Index != i+1 || record.Route != nil || string(record.Body) != expectedBodies[i] || record.Status == nil || *record.Status != expectedStatuses[i] || resp.StatusCode != expectedStatuses[i] {
			t.Fatalf("request %s: status=%d record=%+v", body, resp.StatusCode, record)
		}
		if _, err := time.Parse(time.RFC3339Nano, record.ReceivedAt); err != nil {
			t.Fatalf("request %s: %v", body, err)
		}
	}
	records := recordedRequests(t, recorder)
	if records[0].ResponseIndex == nil || *records[0].ResponseIndex != 0 || records[0].Exhausted || records[1].ResponseIndex == nil || *records[1].ResponseIndex != 1 || records[2].ResponseIndex != nil || !records[2].Exhausted || records[3].ResponseIndex != nil {
		t.Fatalf("records=%+v", records)
	}
}

func TestInvalidRequestsDoNotAdvanceCounter(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid-json", `broken`}, {"wrong-type", `{"stream":"yes"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testServer(t, loadConfig(t, "responses: [{status: 429}]"))
			status, body := postJSON(t, ts.URL, tc.body)
			if status != 400 || !strings.Contains(body, "invalid request body") {
				t.Fatalf("request %s: %d %s", tc.body, status, body)
			}
			if status, body := postJSON(t, ts.URL, `{}`); status != 429 {
				t.Fatalf("valid request after %s: %d %s", tc.body, status, body)
			}
		})
	}
}

func TestRoutesAndIndependentCounters(t *testing.T) {
	ts, recorder := recordingServer(t, loadConfig(t, `routes:
  - name: title
    match: {model: gpt, system_contains: Generate, last_user_contains: hello, has_tools: true}
    responses: [{status: 429}, {status: 503}]
  - name: no-tools
    match: {has_tools: false}
    on_exhausted: loop
    responses: [{status: 401}]
responses: [{status: 502}, {status: 502}]
`))
	cases := []struct {
		name, body, route string
		status, index     int
		exhausted         bool
	}{
		{"system", `{"model":"gpt","messages":[{"role":"system","content":"Generate"},{"role":"user","content":"hello"}],"tools":[{}]}`, "title", 429, 0, false},
		{"model-fallthrough", `{"model":"other","tools":[{}]}`, "", 502, 0, false},
		{"developer-parts", `{"model":"gpt","messages":[{"role":"developer","content":[{"type":"image_url","text":"ignored"},{"type":"text","text":"Generate"},{"type":"text","text":"a title"}]},{"role":"user","content":"old"},{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[{}]}`, "title", 503, 1, false},
		{"no-tools", `{}`, "no-tools", 401, 0, false},
		{"loop", `{}`, "no-tools", 401, 0, true},
		{"last-user-fallthrough", `{"model":"gpt","messages":[{"role":"system","content":"Generate"},{"role":"user","content":"hello"},{"role":"user","content":"bye"}],"tools":[{}]}`, "", 502, 1, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postJSON(t, ts.URL, tc.body)
			if status != tc.status {
				t.Fatalf("request %s: %d %s, want %d", tc.body, status, body, tc.status)
			}
			records := recordedRequests(t, recorder)
			if len(records) != i+1 {
				t.Fatalf("request %s: records=%+v", tc.body, records)
			}
			record := records[i]
			route := ""
			if record.Route != nil {
				route = *record.Route
			}
			if route != tc.route || (tc.route == "" && record.Route != nil) || record.ResponseIndex == nil || *record.ResponseIndex != tc.index || record.Exhausted != tc.exhausted {
				t.Fatalf("request %s: record=%+v", tc.body, record)
			}
		})
	}
}

func TestNoRoute(t *testing.T) {
	ts, recorder := recordingServer(t, loadConfig(t, `routes: [{name: only, match: {model: match}, responses: [{status: 429}]}]`))
	status, body := postJSON(t, ts.URL, `{}`)
	if status != 500 || !strings.Contains(body, "mock_no_route") || !strings.Contains(body, "only") {
		t.Fatalf("unmatched request: %d %s", status, body)
	}
	records := recordedRequests(t, recorder)
	if len(records) != 1 || records[0].Route != nil || records[0].ResponseIndex != nil || records[0].Status == nil || *records[0].Status != 500 {
		t.Fatalf("unmatched request log=%+v", records)
	}
}

type tokenUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
}

func TestUsageOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, usage, body string
		want              tokenUsage
	}{
		{"derived-total", `{prompt_tokens: 7, completion_tokens: 0}`, `{}`, tokenUsage{7, 0, 7}},
		{"stream-derived-total", `{prompt_tokens: 7, completion_tokens: 0}`, `{"stream":true,"stream_options":{"include_usage":true}}`, tokenUsage{7, 0, 7}},
		{"explicit-total", `{prompt_tokens: 2, total_tokens: 99}`, `{}`, tokenUsage{2, 5, 99}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testServer(t, loadConfig(t, `responses: [{file: reply.txt, usage: `+tc.usage+`}]`))
			status, body := postJSON(t, ts.URL, tc.body)
			if status != 200 {
				t.Fatalf("request %s: %d %s", tc.body, status, body)
			}
			var got tokenUsage
			for _, payload := range strings.Split(body, "\n\n") {
				payload = strings.TrimPrefix(payload, "data: ")
				if strings.TrimSpace(payload) == "" || payload == "[DONE]" {
					continue
				}
				var response struct {
					Usage *tokenUsage `json:"usage"`
				}
				if err := json.Unmarshal([]byte(payload), &response); err != nil {
					t.Fatalf("request %s: payload %s: %v", tc.body, payload, err)
				}
				if response.Usage != nil {
					got = *response.Usage
				}
			}
			if got != tc.want {
				t.Fatalf("request %s: usage=%+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

func TestResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, spec, body, header, value string
		status                          int
	}{
		{"json", `file: reply.txt, headers: {X-Mock: yes}`, `{}`, "X-Mock", "yes", 200},
		{"stream", `file: reply.txt, headers: {X-Mock: yes}`, `{"stream":true}`, "X-Mock", "yes", 200},
		{"status", `status: 429, headers: {Retry-After: "2"}`, `{}`, "Retry-After", "2", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := testServer(t, loadConfig(t, `responses: [{`+tc.spec+`}]`))
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("request %s: %v", tc.body, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status || resp.Header.Get(tc.header) != tc.value {
				t.Fatalf("request %s: status=%d headers=%v", tc.body, resp.StatusCode, resp.Header)
			}
		})
	}
}

func TestResponseStreamOverrides(t *testing.T) {
	ts := testServer(t, loadConfig(t, `stream: {chunk_size: 5, chunk_delay: 0s}
responses: [{file: reply.txt, stream: {chunk_size: 2, chunk_delay: 30ms}}]`))
	start := time.Now()
	_, body := postJSON(t, ts.URL, `{"stream":true}`)
	elapsed := time.Since(start)
	for _, piece := range []string{`"content":"he"`, `"content":"ll"`, `"content":"o"`, `data: [DONE]`} {
		if !strings.Contains(body, piece) {
			t.Fatalf("stream request missing %s in %s", piece, body)
		}
	}
	events := strings.Count(body, "data: {")
	if minimum := time.Duration(events-1) * 30 * time.Millisecond; elapsed < minimum {
		t.Fatalf("stream request: elapsed=%v, want >=%v for %d events", elapsed, minimum, events)
	}
}

func TestAbortResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantEvents int
	}{
		{"stream", `{"stream":true}`, 2}, {"non-stream", `{}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, recorder := recordingServer(t, loadConfig(t, `responses: [{file: reply.txt, abort_after_chunks: 2, stream: {chunk_size: 1}}]`))
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
			var body []byte
			if resp != nil {
				body, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if err == nil || strings.Count(string(body), "data: ") != tc.wantEvents || strings.Contains(string(body), "[DONE]") || strings.Contains(string(body), `"finish_reason":"stop"`) {
				t.Fatalf("request %s: body=%s err=%v", tc.body, body, err)
			}
			records := recordedRequests(t, recorder)
			if len(records) != 1 || records[0].Status != nil {
				t.Fatalf("aborted request %s: log=%+v", tc.body, records)
			}
		})
	}
}

func TestModels(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		ids          []string
	}{
		{"default", "", []string{"llmock"}}, {"configured", "models: [gpt-a, gpt-b]\n", []string{"gpt-a", "gpt-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, recorder := recordingServer(t, loadConfig(t, tc.config+"responses: [{status: 500}]"))
			resp, err := http.Get(ts.URL + "/v1/models")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var list struct {
				Object string `json:"object"`
				Data   []struct {
					ID      string `json:"id"`
					Object  string `json:"object"`
					Created int64  `json:"created"`
					OwnedBy string `json:"owned_by"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				t.Fatal(err)
			}
			if list.Object != "list" || len(list.Data) != len(tc.ids) {
				t.Fatalf("GET /v1/models: list=%+v", list)
			}
			for i, model := range list.Data {
				if model.ID != tc.ids[i] || model.Object != "model" || model.OwnedBy != "llmock" {
					t.Fatalf("GET /v1/models: model=%+v", model)
				}
			}
			if got := recordedRequests(t, recorder); len(got) != 0 {
				t.Fatalf("GET /v1/models recorded: %+v", got)
			}
		})
	}
}

func TestCombinedStreamEmitsTextBeforeTools(t *testing.T) {
	ts := testServer(t, loadConfig(t, `responses: [{file: reply.txt, tool_call: call.json}]`))
	_, body := postJSON(t, ts.URL, `{"stream":true}`)
	text := strings.Index(body, `"content":"hello"`)
	tool := strings.Index(body, `"tool_calls":[`)
	if text < 0 || tool < text || !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("combined stream request: %s", body)
	}
}

func TestRoutePoliciesAndFixedMode(t *testing.T) {
	for _, tc := range []struct {
		mode         string
		title, empty []int
	}{
		{"sequence", []int{429, 503, 503}, []int{401, 500}},
		{"fixed", []int{429, 429, 429}, []int{401, 401}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ts := testServer(t, loadConfig(t, `mode: `+tc.mode+`
on_exhausted: repeat_last
routes:
  - name: title
    match: {system_contains: "Generate\na title", has_tools: false}
    responses: [{status: 429}, {status: 503}]
  - name: empty-model
    match: {model: ""}
    on_exhausted: error
    responses: [{status: 401}]
`))
			title := `{"messages":[{"role":"system","content":"Generate"},{"role":"developer","content":"a title"}]}`
			for i, expected := range tc.title {
				if status, raw := postJSON(t, ts.URL, title); status != expected {
					t.Fatalf("title request %d %s: %d %s, want %d", i+1, title, status, raw, expected)
				}
			}
			for i, expected := range tc.empty {
				if status, raw := postJSON(t, ts.URL, `{}`); status != expected {
					t.Fatalf("empty request %d: %d %s, want %d", i+1, status, raw, expected)
				}
			}

		})
	}
}

func TestConcurrentRouteLoops(t *testing.T) {
	ts, recorder := recordingServer(t, loadConfig(t, `on_exhausted: loop
routes:
  - name: a
    match: {model: a}
    responses: [{status: 429}, {status: 503}]
  - name: b
    match: {model: b}
    responses: [{status: 429}, {status: 503}]
`))
	const count = 24
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		model := []string{"a", "b"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":%q}`, model)
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("parallel request %s: %v", body, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 429 && resp.StatusCode != 503 {
				t.Errorf("parallel request %s: status=%d", body, resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	records := recordedRequests(t, recorder)
	if len(records) != count {
		t.Fatalf("parallel requests: log length=%d, want %d", len(records), count)
	}
	seen := make(map[int]bool)
	counts := map[string]int{"a": 0, "b": 0}
	for i, record := range records {
		if record.Index != i+1 || seen[record.Index] || record.Index < 1 || record.Index > count || record.Route == nil {
			t.Fatalf("parallel requests: invalid record=%+v", record)
		}
		seen[record.Index] = true
		route := *record.Route
		prior, ok := counts[route]
		if !ok || record.ResponseIndex == nil || *record.ResponseIndex != prior%2 || record.Exhausted != (prior >= 2) {
			t.Fatalf("parallel request route=%s occurrence=%d: record=%+v", route, prior, record)
		}
		counts[route]++
	}
	if counts["a"] != count/2 || counts["b"] != count/2 {
		t.Fatalf("parallel per-route counts=%v", counts)
	}
}

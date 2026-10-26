package main

import (
	"embed"
	"fmt"
)

//go:embed examples/*
var exampleFiles embed.FS

const rootHelp = `llmock - mock OpenAI Chat Completions server for E2E tests

Quick start:
  llmock serve response.txt
  llmock serve -c llmock.yaml
  Read the port from the first line of stdout; use this client base URL:
  http://127.0.0.1:<port>/v1

Commands:
  serve      Start the mock server
  validate   Check a config and its referenced files without serving
  version    Print version, commit, and build date
  help       Show help for a command or topic

Help topics:
  config     YAML fields, defaults, response files, and streaming
  routes     Request matching and independent response sequences
  record     JSONL request records and status meanings
  errors     Error types, HTTP statuses, and causes
  example    Complete example files and an E2E harness flow

Run 'llmock help <command|topic>' for details.
`

const serveHelp = `Start a mock OpenAI Chat Completions server.

Usage:
  llmock serve response.txt [options]
  llmock serve -c llmock.yaml [options]

Startup contract:
  The first stdout line is the bound port; logs go to stderr.
  SIGINT or SIGTERM shuts down the server (up to 2 seconds for graceful shutdown).
  Restarting resets all sequences and truncates the --record file.
  A text file repeats forever. YAML controls routes and response sequences.
  The request's "stream": true selects SSE; otherwise the response is JSON.

Endpoints:
  POST /v1/chat/completions   Mock completion (request body limit: 32 MiB)
  GET  /v1/models             Configured model list
  GET  /health                Readiness probe (200, empty body)

Options (unset overrides preserve config values):
`

const validateHelp = `Check a config and all referenced text/tool-call files without starting a server.

Usage:
  llmock validate -c llmock.yaml
  llmock validate response.txt

Checks strict YAML keys, field constraints, routes, readable referenced files,
and tool-call JSON. Text-only mode checks that the supplied file is readable.
This does not bind a port or verify that the configured host can be used.

Exit codes:
  0  Valid configuration
  1  Configuration/file error
  2  Usage error (missing input, conflicting inputs, or invalid flags)

Options:
`

var helpTopics = map[string]string{
	"help": rootHelp,
	"version": `Print version information.

Usage:
  llmock version
  llmock --version
  llmock -v

Output: llmock <version> (commit <commit>, built <date>)
`,
	"config": configHelp,
	"routes": routesHelp,
	"record": recordHelp,
	"errors": errorsHelp,
}

const configHelp = `YAML configuration reference

Use 'llmock serve -c llmock.yaml' or 'llmock validate -c llmock.yaml'.
Unknown YAML keys are errors. Referenced paths are relative to the config file.

Top-level fields:
  host          string; default 127.0.0.1; listen host (resolved at startup).
  port          integer; default 0 (free port); range 0..65535.
  models        string list; default [llmock], also when empty; GET /v1/models only.
  mode          string; default sequence; sequence advances, fixed repeats first.
  on_exhausted  string; default error; error | repeat_last | loop.
  stream        mapping; default {chunk_size: 5, chunk_delay: 0s}; see below.
  routes        route list; default empty; see 'llmock help routes'.
  responses     response list; default empty; required unless routes are present.

In sequence mode, error returns 500 after the last response, repeat_last repeats
it (with a warning), and loop wraps to the beginning. Fixed mode always selects
response 0 in each sequence and never exhausts, regardless of on_exhausted.

Response fields (in top-level or route responses):
  file                string path; default absent; read as literal response text.
  tool_call           string path; default absent; load tool-call JSON (below).
  status              integer; default 0 (absent); injected error, range 400..599.
  delay               duration string; default 0s; >= 0, e.g. 500ms or 1s.
  usage               mapping of token-count integers; defaults described below.
  headers             string-to-string mapping; default empty; HTTP headers.
  abort_after_chunks  integer; default absent; if set, >= 1; no status allowed.
  stream              mapping; default inherit global settings; no status allowed.

Supply file and/or tool_call, or status alone (no file/tool_call).
Status responses may still set delay, usage, and headers. Negative delays, usage
counts, and stream delays are rejected. Explicit status: 0 means absent.
Headers must have valid HTTP token names and values without CR or LF.
Content-Type, Content-Length, Transfer-Encoding, and Connection are forbidden
(case-insensitive). Other headers are applied to the selected response.

Streaming settings (global stream or response stream):
  chunk_size   integer >= 1; explicit 0 is rejected; counts Unicode code points.
  chunk_delay  duration string >= 0; delay between JSON SSE data events.
Response stream fields override only the fields supplied, including a 0s delay.
Streaming is selected by the client's stream flag, not by this configuration.
SSE sends an initial assistant role event, text, tool-call headers/arguments,
a finish event, an optional usage event, then data: [DONE].
abort_after_chunks counts JSON SSE data events, including the initial role and
optional usage event, not [DONE]; it disconnects after that event. A threshold
beyond all events completes normally with a warning. Non-stream requests with
abort_after_chunks disconnect without a response.

Usage fields (each optional, nonnegative integer):
  prompt_tokens      default 0.
  completion_tokens  default character count of text plus tool-call arguments.
  total_tokens       default prompt_tokens + completion_tokens.
These are mock counts, not tokenizer estimates. Non-stream replies include usage.
Streams send a usage chunk only if the client sets stream_options.include_usage
true; that chunk has choices: [].

Tool-call file format (object or array of objects):
  {"name":"get_weather","arguments":{"city":"Seoul"}}
  [{"name":"get_weather","arguments":"{\"city\":\"Seoul\"}"}]
name is required and nonempty. arguments defaults to {} when omitted. An object
is compacted to JSON; a JSON string is used verbatim (its contents are not
validated). Numbers, booleans, and arrays are also accepted and compacted; null
becomes an empty argument string. An empty array of calls produces no tool calls.
Tool-call JSON keys, unlike YAML configuration keys, are not checked strictly.

Replies have role assistant. With tool calls, finish_reason is tool_calls;
otherwise it is stop. Non-stream content is null when tool calls exist and text
is empty; otherwise it is a string. file + tool_call returns both. Tool arguments
are strings in the API and are split into deltas when streaming.
The requested model is echoed (default llmock); models does not restrict requests.

Text-only mode:
  llmock serve response.txt
is equivalent to YAML with mode: fixed and one response with file: response.txt
(all other fields use defaults). No extension check is performed on the input.

See also: llmock help routes | record | errors | example
`

const routesHelp = `Routes and response selection

Each route requires a nonempty unique name, a match mapping with at least one
condition, and a nonempty responses list. Conditions are ANDed. First matching
route wins in YAML order; omitted conditions do not constrain the request.

Match fields:
  model               string; exact match against the request model, even empty.
  system_contains     nonempty string; case-sensitive substring of all system
                      and developer messages joined in order with \n.
  last_user_contains  nonempty string; case-sensitive substring of the last user
                      message only, not earlier user messages.
  has_tools           boolean; true if the request tools array is nonempty;
                      false for absent, null, or empty tools (not tool_choice).

Message content may be a string or an array of parts. Only parts with type: text
contribute their text, joined by \n; other content contributes an empty string.
System/developer messages are then joined by \n in their original order.

Each route and the default responses have independent counters. mode applies to
all sequences. A route's on_exhausted overrides the top-level policy; if omitted,
it inherits that policy. An exhausted matching route does not fall through.
Unmatched requests use top-level responses, or return 500 mock_no_route if absent.
Malformed requests do not consume responses. Selected errors and aborts do.

Concurrent requests hitting the same route race for response order. Give parallel
tests distinct match conditions so each uses its own route and sequence.

Example:
  on_exhausted: error
  routes:
    - name: title
      match:
        system_contains: Generate a title
        has_tools: false
      on_exhausted: repeat_last
      responses:
        - file: answer.txt
    - name: weather
      match: {model: weather-test, has_tools: true}
      responses:
        - tool_call: weather_call.json
  responses:
    - file: hello.txt

See also: llmock help config | record | errors
`

const recordHelp = `Request recording

Use 'llmock serve -c llmock.yaml --record requests.jsonl'. The JSONL file is
created or truncated at startup. Only POST /v1/chat/completions is recorded,
including invalid requests. Each line is written before sending the response,
under the same lock as selection; line order equals index order, not necessarily
arrival order or completion order. Restart resets indices and sequences.

Fields (all keys are always present):
  index           integer; global 1-based request number.
  received_at     string; UTC RFC3339Nano time captured before reading the body.
  route           string|null; matched route name; null for default/no selection.
  response_index  integer|null; 0-based index within the selected response list;
                  null for invalid bodies, no route, or exhaustion with error.
  exhausted       boolean; true after the first pass in sequence mode, including
                  loop/repeat_last; false in fixed mode or before selection.
  body            JSON value; original valid JSON, or a JSON string containing
                  invalid raw body bytes (up to the 32 MiB request limit).
  status          integer|null; intended response status, before network writes.

Status values:
  200      Normal selected completion (even if later cancelled/write fails).
  400..599 Injected response status (including injected 400 or 500).
  400      Invalid JSON, incompatible request field types, or body read failure.
  500      No matching route/default responses, or responses exhausted (error).
  null     Selected abort_after_chunks, even if threshold is never reached.

Records describe selection, not final transport outcome. For example, a stream
unsupported by a connection is recorded as 200 but returns 500 server_error.
A record write failure returns 500 mock_record_error instead of the selected
response; the line may be missing/partial and a selected response is consumed.

See also: llmock help routes | errors
`

const errorsHelp = `HTTP errors

Completion errors have this JSON shape:
  {"error":{"message":"...","type":"...","param":null,"code":null}}

HTTP status  error.type                When
400          invalid_request_error     Invalid JSON, incompatible request field
                                      types, body read failure or >32 MiB body.
400..599     mock_error                Injected response status from YAML.
500          mock_responses_exhausted  Selected sequence exhausted with error.
500          mock_no_route             No route matched and no default responses.
500          mock_record_error         Failed to write a request record.
500          server_error              Streaming connection lacks HTTP flushing.

Missing model/messages are accepted; unknown request fields are ignored.
Unsupported paths/methods use standard HTTP 404/405 responses, not this JSON
error envelope. Config/startup failures go to stderr, not HTTP responses.
An injected abort disconnects without a JSON error. Non-stream aborts send no
response; stream aborts leave a partial SSE response without [DONE].

See also: llmock help config | record
`

func printExample() {
	fmt.Print("Complete example files (save in the same directory):\n")
	for _, name := range []string{"llmock.yaml", "hello.txt", "answer.txt", "weather_call.json"} {
		data, err := exampleFiles.ReadFile("examples/" + name)
		if err != nil {
			panic(err)
		}
		fmt.Printf("\n--- %s ---\n%s\n", name, data)
	}
	fmt.Print(`
E2E harness flow:
  1. Save the files above and validate: llmock validate -c llmock.yaml
  2. Start: llmock serve -c llmock.yaml --port 0 --record requests.jsonl
  3. Read the first stdout line as the port; keep stderr separate.
  4. Point your OpenAI-compatible client at http://127.0.0.1:<port>/v1.
  5. Run requests/tests. The example has independent title and default sequences.
  6. Read requests.jsonl to assert request bodies, routes, and response indices.
  7. Send SIGINT or SIGTERM and wait for exit. Restart for a fresh run.

See also: llmock help config | routes | record | errors
`)
}

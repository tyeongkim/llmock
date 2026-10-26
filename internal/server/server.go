package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tyeongkim/llmock/internal/config"
)

type Server struct {
	cfg config.Config

	mu              sync.Mutex
	defaultSequence sequence
	routeSequences  []sequence
	recorder        io.Writer
	nextRequest     int
}

type sequence struct {
	responses []config.Response
	policy    config.OnExhausted
	next      int
}

type takenResponse struct {
	response  config.Response
	index     *int
	exhausted bool
}

func New(cfg config.Config, recorder io.Writer) *Server {
	s := &Server{cfg: cfg, recorder: recorder, defaultSequence: sequence{responses: cfg.Responses, policy: cfg.OnExhausted}}
	for _, route := range cfg.Routes {
		s.routeSequences = append(s.routeSequences, sequence{responses: route.Responses, policy: route.OnExhausted})
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		models := s.cfg.Models
		data := make([]map[string]any, 0, len(models))
		for _, id := range models {
			data = append(data, map[string]any{"id": id, "object": "model", "created": createdAt, "owned_by": "llmock"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

type chatRequest struct {
	Model         string            `json:"model"`
	Stream        bool              `json:"stream"`
	Messages      []requestMessage  `json:"messages"`
	Tools         []json.RawMessage `json:"tools"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now().UTC().Format(time.RFC3339Nano)
	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	var request chatRequest
	err := json.Unmarshal(raw, &request)
	if readErr != nil {
		err = readErr
	}
	system, lastUser := requestText(request.Messages)
	record := requestRecord{ReceivedAt: receivedAt}
	if s.recorder != nil {
		record.Body = json.RawMessage(raw)
		if !json.Valid(raw) {
			record.Body, _ = json.Marshal(string(raw))
		}
	}

	s.mu.Lock()
	s.nextRequest++
	record.Index = s.nextRequest
	status := http.StatusBadRequest
	record.Status = &status
	var response config.Response
	errorType, errorMessage := "invalid_request_error", ""
	if err != nil {
		errorMessage = "invalid request body: " + err.Error()
	} else {
		response, errorType, errorMessage = s.selectResponse(request, system, lastUser, &record)
	}
	var recordErr error
	if s.recorder != nil {
		recordErr = json.NewEncoder(s.recorder).Encode(record)
	}
	s.mu.Unlock()
	if recordErr != nil {
		log.Printf("record request #%d: %v", record.Index, recordErr)
		writeError(w, http.StatusInternalServerError, "mock_record_error", "llmock: could not record request")
		return
	}
	if errorType != "" {
		writeError(w, *record.Status, errorType, errorMessage)
		return
	}
	log.Printf("request #%d -> response %s", record.Index, describe(response))

	if !sleep(r, response.Delay) {
		return
	}

	for name, value := range response.Headers {
		w.Header().Set(name, value)
	}
	if response.Status != 0 {
		writeError(w, response.Status, "mock_error", fmt.Sprintf("llmock: injected HTTP %d", response.Status))
		return
	}

	model := request.Model
	if model == "" {
		model = "llmock"
	}
	completionID := fmt.Sprintf("chatcmpl-llmock-%d", record.Index)

	if request.Stream {
		includeUsage := request.StreamOptions != nil && request.StreamOptions.IncludeUsage
		s.writeStream(w, r, completionID, model, response, includeUsage)
		return
	}
	if response.AbortAfterChunks > 0 {
		log.Printf("abort non-stream completion %s", completionID)
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, http.StatusOK, buildCompletion(completionID, model, response))
}

type requestMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func messageText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func requestText(messages []requestMessage) (string, string) {
	var system []string
	var lastUser string
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			system = append(system, messageText(message.Content))
		case "user":
			lastUser = messageText(message.Content)
		}
	}
	return strings.Join(system, "\n"), lastUser
}

func matches(match config.Match, request chatRequest, system, lastUser string) bool {
	if match.Model != nil && *match.Model != request.Model {
		return false
	}
	if match.HasTools != nil && *match.HasTools != (len(request.Tools) > 0) {
		return false
	}
	return (match.SystemContains == nil || strings.Contains(system, *match.SystemContains)) &&
		(match.LastUserContains == nil || strings.Contains(lastUser, *match.LastUserContains))
}

type requestRecord struct {
	Index         int             `json:"index"`
	ReceivedAt    string          `json:"received_at"`
	Route         *string         `json:"route"`
	ResponseIndex *int            `json:"response_index"`
	Exhausted     bool            `json:"exhausted"`
	Body          json.RawMessage `json:"body"`
	Status        *int            `json:"status"`
}

func (s *Server) selectResponse(request chatRequest, system, lastUser string, record *requestRecord) (config.Response, string, string) {
	selected := &s.defaultSequence
	for i, route := range s.cfg.Routes {
		if !matches(route.Match, request, system, lastUser) {
			continue
		}
		record.Route = &route.Name
		selected = &s.routeSequences[i]
		break
	}
	status := http.StatusInternalServerError
	record.Status = &status
	if len(selected.responses) == 0 {
		names := make([]string, 0, len(s.cfg.Routes))
		for _, route := range s.cfg.Routes {
			names = append(names, route.Name)
		}
		return config.Response{}, "mock_no_route", "llmock: no matching route; configured routes: " + strings.Join(names, ", ")
	}
	taken, ok := selected.take(s.cfg.Mode == config.ModeFixed)
	record.ResponseIndex, record.Exhausted = taken.index, taken.exhausted
	if !ok {
		return config.Response{}, "mock_responses_exhausted", fmt.Sprintf("llmock: all %d configured responses were used", len(selected.responses))
	}
	if taken.exhausted && selected.policy == config.OnExhaustedRepeatLast {
		log.Printf("warning: request #%d served past the end of the script", record.Index)
	}
	status = http.StatusOK
	if taken.response.Status != 0 {
		status = taken.response.Status
	}
	if taken.response.AbortAfterChunks > 0 {
		record.Status = nil
	}
	return taken.response, "", ""
}

func (s *sequence) take(fixed bool) (takenResponse, bool) {
	index := s.next
	s.next++
	result := takenResponse{}
	if fixed {
		index = 0
	} else if index >= len(s.responses) {
		result.exhausted = true
		switch s.policy {
		case config.OnExhaustedLoop:
			index %= len(s.responses)
		case config.OnExhaustedRepeatLast:
			index = len(s.responses) - 1
		default:
			return result, false
		}
	}
	result.response, result.index = s.responses[index], &index
	return result, true
}

func sleep(r *http.Request, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	select {
	case <-time.After(delay):
		return true
	case <-r.Context().Done():
		return false
	}
}

func describe(response config.Response) string {
	switch {
	case response.Status != 0:
		return fmt.Sprintf("status=%d", response.Status)
	case len(response.ToolCalls) > 0:
		return fmt.Sprintf("tool_calls=%d", len(response.ToolCalls))
	default:
		return fmt.Sprintf("content=%d bytes", len(response.Content))
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"param":   nil,
			"code":    nil,
		},
	})
}

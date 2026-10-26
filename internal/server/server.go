package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/tyeongkim/llmock/internal/config"
)

type Server struct {
	cfg config.Config

	mu          sync.Mutex
	nextRequest int
}

func New(cfg config.Config) *Server {
	return &Server{cfg: cfg}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /__admin/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.nextRequest = 0
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

type chatRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var request chatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON: "+err.Error())
		return
	}

	requestNumber, response, ok := s.take()
	if !ok {
		writeError(w, http.StatusInternalServerError, "mock_responses_exhausted",
			fmt.Sprintf("llmock: all %d configured responses were used", len(s.cfg.Responses)))
		return
	}
	log.Printf("request #%d -> response %s", requestNumber+1, describe(response))

	if !sleep(r, response.Delay) {
		return
	}

	if response.Status != 0 {
		writeError(w, response.Status, "mock_error", fmt.Sprintf("llmock: injected HTTP %d", response.Status))
		return
	}

	model := request.Model
	if model == "" {
		model = "llmock"
	}
	completionID := fmt.Sprintf("chatcmpl-llmock-%d", requestNumber+1)

	if request.Stream {
		includeUsage := request.StreamOptions != nil && request.StreamOptions.IncludeUsage
		s.writeStream(w, r, completionID, model, response, includeUsage)
		return
	}
	writeJSON(w, http.StatusOK, buildCompletion(completionID, model, response))
}

func (s *Server) take() (int, config.Response, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	requestNumber := s.nextRequest
	s.nextRequest++
	responses := s.cfg.Responses

	if s.cfg.Mode == config.ModeFixed {
		return requestNumber, responses[0], true
	}
	if requestNumber < len(responses) {
		return requestNumber, responses[requestNumber], true
	}

	switch s.cfg.OnExhausted {
	case config.OnExhaustedLoop:
		return requestNumber, responses[requestNumber%len(responses)], true
	case config.OnExhaustedError:
		return requestNumber, config.Response{}, false
	default:
		return requestNumber, responses[len(responses)-1], true
	}
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

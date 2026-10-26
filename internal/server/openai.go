package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/tyeongkim/llmock/internal/config"
)

const createdAt = 1700000000

type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usage              `json:"usage"`
}

type completionChoice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type message struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

func finishReason(response config.Response) string {
	if len(response.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func toolCallID(completionID string, index int) string {
	return fmt.Sprintf("call_%s_%d", completionID, index)
}

func countUsage(response config.Response) usage {
	completionTokens := len([]rune(response.Content))
	for _, call := range response.ToolCalls {
		completionTokens += len([]rune(call.Arguments))
	}
	return usage{CompletionTokens: completionTokens, TotalTokens: completionTokens}
}

func buildCompletion(completionID, model string, response config.Response) completion {
	reply := message{Role: "assistant"}
	if len(response.ToolCalls) == 0 {
		reply.Content = &response.Content
	}
	for i, call := range response.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, toolCall{
			ID:       toolCallID(completionID, i),
			Type:     "function",
			Function: toolFunction{Name: call.Name, Arguments: call.Arguments},
		})
	}

	return completion{
		ID:      completionID,
		Object:  "chat.completion",
		Created: createdAt,
		Model:   model,
		Choices: []completionChoice{{Message: reply, FinishReason: finishReason(response)}},
		Usage:   countUsage(response),
	}
}

func (s *Server) writeStream(w http.ResponseWriter, r *http.Request, completionID, model string, response config.Response, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming is not supported by this connection")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	first := true
	send := func(choices []chunkChoice, chunkUsage *usage) bool {
		if !first && !sleep(r, s.cfg.Stream.ChunkDelay) {
			return false
		}
		first = false

		payload, err := json.Marshal(chunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: createdAt,
			Model:   model,
			Choices: choices,
			Usage:   chunkUsage,
		})
		if err != nil {
			log.Printf("encode chunk: %v", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	sendDelta := func(d delta) bool {
		return send([]chunkChoice{{Delta: d}}, nil)
	}

	empty := ""
	if !sendDelta(delta{Role: "assistant", Content: &empty}) {
		return
	}

	chunkSize := s.cfg.Stream.ChunkSize
	if len(response.ToolCalls) == 0 {
		for _, piece := range splitRunes(response.Content, chunkSize) {
			if !sendDelta(delta{Content: &piece}) {
				return
			}
		}
	}

	for i, call := range response.ToolCalls {
		index := i
		header := toolCall{
			Index:    &index,
			ID:       toolCallID(completionID, i),
			Type:     "function",
			Function: toolFunction{Name: call.Name},
		}
		if !sendDelta(delta{ToolCalls: []toolCall{header}}) {
			return
		}
		for _, piece := range splitRunes(call.Arguments, chunkSize) {
			argumentPiece := toolCall{Index: &index, Function: toolFunction{Arguments: piece}}
			if !sendDelta(delta{ToolCalls: []toolCall{argumentPiece}}) {
				return
			}
		}
	}

	reason := finishReason(response)
	if !send([]chunkChoice{{Delta: delta{}, FinishReason: &reason}}, nil) {
		return
	}
	if includeUsage {
		total := countUsage(response)
		if !send([]chunkChoice{}, &total) {
			return
		}
	}

	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

func splitRunes(text string, size int) []string {
	runes := []rune(text)
	pieces := make([]string, 0, len(runes)/size+1)
	for start := 0; start < len(runes); start += size {
		end := min(start+size, len(runes))
		pieces = append(pieces, string(runes[start:end]))
	}
	return pieces
}

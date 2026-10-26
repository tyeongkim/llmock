package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	ModeSequence Mode = "sequence"
	ModeFixed    Mode = "fixed"
)

type OnExhausted string

const (
	OnExhaustedRepeatLast OnExhausted = "repeat_last"
	OnExhaustedLoop       OnExhausted = "loop"
	OnExhaustedError      OnExhausted = "error"
)

type Config struct {
	Host        string
	Port        int
	Mode        Mode
	OnExhausted OnExhausted
	Stream      Stream
	Responses   []Response
	Routes      []Route
	Models      []string
}

type Match struct {
	Model            *string `yaml:"model"`
	SystemContains   *string `yaml:"system_contains"`
	LastUserContains *string `yaml:"last_user_contains"`
	HasTools         *bool   `yaml:"has_tools"`
}

type Route struct {
	Name        string
	Match       Match
	OnExhausted OnExhausted
	Responses   []Response
}

type routeSpec struct {
	Name        string         `yaml:"name"`
	Match       Match          `yaml:"match"`
	OnExhausted OnExhausted    `yaml:"on_exhausted"`
	Responses   []responseSpec `yaml:"responses"`
}

type Stream struct {
	ChunkSize  int           `yaml:"chunk_size"`
	ChunkDelay time.Duration `yaml:"chunk_delay"`
}

type Usage struct {
	PromptTokens     *int `yaml:"prompt_tokens"`
	CompletionTokens *int `yaml:"completion_tokens"`
	TotalTokens      *int `yaml:"total_tokens"`
}

type ResponseStream struct {
	ChunkSize  *int           `yaml:"chunk_size"`
	ChunkDelay *time.Duration `yaml:"chunk_delay"`
}

type Response struct {
	Usage            Usage
	Headers          map[string]string
	AbortAfterChunks int
	Stream           *ResponseStream
	Content          string
	Status           int
	ToolCalls        []ToolCall
	Delay            time.Duration
}

type ToolCall struct {
	Name      string
	Arguments string
}

type fileConfig struct {
	Host        string         `yaml:"host"`
	Port        int            `yaml:"port"`
	Mode        Mode           `yaml:"mode"`
	OnExhausted OnExhausted    `yaml:"on_exhausted"`
	Stream      ResponseStream `yaml:"stream"`
	Responses   []responseSpec `yaml:"responses"`
	Routes      []routeSpec    `yaml:"routes"`
	Models      []string       `yaml:"models"`
}

type responseSpec struct {
	Usage            Usage             `yaml:"usage"`
	Headers          map[string]string `yaml:"headers"`
	AbortAfterChunks *int              `yaml:"abort_after_chunks"`
	Stream           *ResponseStream   `yaml:"stream"`
	File             string            `yaml:"file"`
	Status           int               `yaml:"status"`
	ToolCall         string            `yaml:"tool_call"`
	Delay            time.Duration     `yaml:"delay"`
}

type toolCallFile struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var parsed fileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&parsed); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	cfg := Config{
		Host:        parsed.Host,
		Port:        parsed.Port,
		Mode:        parsed.Mode,
		OnExhausted: parsed.OnExhausted,
		Models:      parsed.Models,
	}
	cfg = WithDefaults(cfg)
	if parsed.Stream.ChunkSize != nil {
		if *parsed.Stream.ChunkSize < 1 {
			return Config{}, fmt.Errorf("%s: stream.chunk_size must be >= 1", path)
		}
		cfg.Stream.ChunkSize = *parsed.Stream.ChunkSize
	}
	if parsed.Stream.ChunkDelay != nil {
		cfg.Stream.ChunkDelay = *parsed.Stream.ChunkDelay
	}
	if err := validate(cfg, parsed.Responses, len(parsed.Routes) > 0); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	baseDir := filepath.Dir(path)
	for i, spec := range parsed.Responses {
		response, err := loadResponse(baseDir, spec)
		if err != nil {
			return Config{}, fmt.Errorf("%s: responses[%d]: %w", path, i, err)
		}
		cfg.Responses = append(cfg.Responses, response)
	}
	names := make(map[string]bool)
	for i, spec := range parsed.Routes {
		location := fmt.Sprintf("routes[%d]", i)
		if spec.Name == "" || names[spec.Name] {
			return Config{}, fmt.Errorf("%s: %s: name must be non-empty and unique", path, location)
		}
		names[spec.Name] = true
		if spec.Match == (Match{}) {
			return Config{}, fmt.Errorf("%s: %s.match: at least one condition is required", path, location)
		}
		if spec.Match.SystemContains != nil && *spec.Match.SystemContains == "" || spec.Match.LastUserContains != nil && *spec.Match.LastUserContains == "" {
			return Config{}, fmt.Errorf("%s: %s.match: contains conditions must not be empty", path, location)
		}
		policy := spec.OnExhausted
		if policy == "" {
			policy = cfg.OnExhausted
		}
		routeConfig := cfg
		routeConfig.OnExhausted = policy
		if err := validate(routeConfig, spec.Responses, false); err != nil {
			return Config{}, fmt.Errorf("%s: %s.%w", path, location, err)
		}
		route := Route{Name: spec.Name, Match: spec.Match, OnExhausted: policy}
		for j, responseSpec := range spec.Responses {
			response, err := loadResponse(baseDir, responseSpec)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %s.responses[%d]: %w", path, location, j, err)
			}
			route.Responses = append(route.Responses, response)
		}
		cfg.Routes = append(cfg.Routes, route)
	}
	return cfg, nil
}

func FromTextFile(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Mode: ModeFixed, Responses: []Response{{Content: string(content)}}}
	return WithDefaults(cfg), nil
}

func WithDefaults(cfg Config) Config {
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"llmock"}
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeSequence
	}
	if cfg.OnExhausted == "" {
		cfg.OnExhausted = OnExhaustedError
	}
	if cfg.Stream.ChunkSize == 0 {
		cfg.Stream.ChunkSize = 5
	}
	return cfg
}

func validate(cfg Config, specs []responseSpec, hasRoutes bool) error {
	switch cfg.Mode {
	case ModeSequence, ModeFixed:
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeSequence, ModeFixed, cfg.Mode)
	}

	switch cfg.OnExhausted {
	case OnExhaustedRepeatLast, OnExhaustedLoop, OnExhaustedError:
	default:
		return fmt.Errorf("on_exhausted must be %q, %q or %q, got %q",
			OnExhaustedRepeatLast, OnExhaustedLoop, OnExhaustedError, cfg.OnExhausted)
	}

	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("port out of range: %d", cfg.Port)
	}
	if cfg.Stream.ChunkSize < 1 {
		return errors.New("stream.chunk_size must be >= 1")
	}
	if cfg.Stream.ChunkDelay < 0 {
		return errors.New("stream.chunk_delay must not be negative")
	}
	if len(specs) == 0 && !hasRoutes {
		return errors.New("responses must not be empty")
	}

	for i, spec := range specs {
		if spec.Delay < 0 {
			return fmt.Errorf("responses[%d]: delay must not be negative", i)
		}
		for name, value := range map[string]*int{"prompt_tokens": spec.Usage.PromptTokens, "completion_tokens": spec.Usage.CompletionTokens, "total_tokens": spec.Usage.TotalTokens} {
			if value != nil && *value < 0 {
				return fmt.Errorf("responses[%d]: usage.%s must not be negative", i, name)
			}
		}
		for name, value := range spec.Headers {
			if !validHeaderName(name) {
				return fmt.Errorf("responses[%d]: headers: invalid header name %q", i, name)
			}
			switch strings.ToLower(name) {
			case "content-type", "content-length", "transfer-encoding", "connection":
				return fmt.Errorf("responses[%d]: headers: %s is reserved", i, name)
			}
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("responses[%d]: headers: %s value must not contain CR or LF", i, name)
			}
		}
		kinds := 0
		if spec.File != "" {
			kinds++
		}
		if spec.Status != 0 {
			kinds++
		}
		if spec.ToolCall != "" {
			kinds++
		}
		if kinds == 0 || (spec.Status != 0 && kinds != 1) {
			return fmt.Errorf("responses[%d]: file and/or tool_call, or status alone is required", i)
		}
		if spec.AbortAfterChunks != nil && (*spec.AbortAfterChunks < 1 || spec.Status != 0) {
			return fmt.Errorf("responses[%d]: abort_after_chunks must be >= 1 and cannot be combined with status", i)
		}
		if spec.Stream != nil {
			if spec.Status != 0 {
				return fmt.Errorf("responses[%d]: stream cannot be combined with status", i)
			}
			if spec.Stream.ChunkSize != nil && *spec.Stream.ChunkSize < 1 {
				return fmt.Errorf("responses[%d]: stream.chunk_size must be >= 1", i)
			}
			if spec.Stream.ChunkDelay != nil && *spec.Stream.ChunkDelay < 0 {
				return fmt.Errorf("responses[%d]: stream.chunk_delay must not be negative", i)
			}
		}
		if spec.Status != 0 && (spec.Status < 400 || spec.Status > 599) {
			return fmt.Errorf("responses[%d]: status must be 400-599, got %d", i, spec.Status)
		}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char) {
			continue
		}
		return false
	}
	return true
}

func loadResponse(baseDir string, spec responseSpec) (Response, error) {
	response := Response{Status: spec.Status, Delay: spec.Delay, Usage: spec.Usage, Headers: spec.Headers, Stream: spec.Stream}
	if spec.AbortAfterChunks != nil {
		response.AbortAfterChunks = *spec.AbortAfterChunks
	}

	if spec.File != "" {
		content, err := os.ReadFile(filepath.Join(baseDir, spec.File))
		if err != nil {
			return Response{}, err
		}
		response.Content = string(content)
	}

	if spec.ToolCall != "" {
		toolCalls, err := loadToolCalls(filepath.Join(baseDir, spec.ToolCall))
		if err != nil {
			return Response{}, err
		}
		response.ToolCalls = toolCalls
	}
	return response, nil
}

func loadToolCalls(path string) ([]ToolCall, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var files []toolCallFile
	if err := json.Unmarshal(raw, &files); err != nil {
		var single toolCallFile
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, fmt.Errorf("%s: expected a tool call object or array: %w", path, err)
		}
		files = []toolCallFile{single}
	}

	toolCalls := make([]ToolCall, 0, len(files))
	for i, file := range files {
		if file.Name == "" {
			return nil, fmt.Errorf("%s: [%d]: name is required", path, i)
		}
		arguments, err := encodeArguments(file.Arguments)
		if err != nil {
			return nil, fmt.Errorf("%s: [%d]: arguments: %w", path, i, err)
		}
		toolCalls = append(toolCalls, ToolCall{Name: file.Name, Arguments: arguments})
	}
	return toolCalls, nil
}

func encodeArguments(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}

	var alreadyEncoded string
	if json.Unmarshal(raw, &alreadyEncoded) == nil {
		return alreadyEncoded, nil
	}

	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return "", err
	}
	return compacted.String(), nil
}

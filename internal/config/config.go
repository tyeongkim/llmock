package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

type Stream struct {
	ChunkSize  int           `yaml:"chunk_size"`
	ChunkDelay time.Duration `yaml:"chunk_delay"`
}

type Response struct {
	Content   string
	Status    int
	ToolCalls []ToolCall
	Delay     time.Duration
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
	Stream      Stream         `yaml:"stream"`
	Responses   []responseSpec `yaml:"responses"`
}

type responseSpec struct {
	File     string        `yaml:"file"`
	Status   int           `yaml:"status"`
	ToolCall string        `yaml:"tool_call"`
	Delay    time.Duration `yaml:"delay"`
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
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	cfg := Config{
		Host:        parsed.Host,
		Port:        parsed.Port,
		Mode:        parsed.Mode,
		OnExhausted: parsed.OnExhausted,
		Stream:      parsed.Stream,
	}
	applyDefaults(&cfg)
	if err := validate(cfg, parsed.Responses); err != nil {
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
	return cfg, nil
}

func FromTextFile(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Responses: []Response{{Content: string(content)}}}
	applyDefaults(&cfg)
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeSequence
	}
	if cfg.OnExhausted == "" {
		cfg.OnExhausted = OnExhaustedRepeatLast
	}
	if cfg.Stream.ChunkSize == 0 {
		cfg.Stream.ChunkSize = 5
	}
}

func validate(cfg Config, specs []responseSpec) error {
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
	if cfg.Stream.ChunkSize < 0 {
		return errors.New("stream.chunk_size must not be negative")
	}
	if len(specs) == 0 {
		return errors.New("responses must not be empty")
	}

	for i, spec := range specs {
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
		if kinds != 1 {
			return fmt.Errorf("responses[%d]: exactly one of file, status, tool_call is required", i)
		}
		if spec.Status != 0 && (spec.Status < 400 || spec.Status > 599) {
			return fmt.Errorf("responses[%d]: status must be 400-599, got %d", i, spec.Status)
		}
	}
	return nil
}

func loadResponse(baseDir string, spec responseSpec) (Response, error) {
	response := Response{Status: spec.Status, Delay: spec.Delay}

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

package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/tyeongkim/llmock/internal/config"
	"github.com/tyeongkim/llmock/internal/server"
)

func newClient(t *testing.T, cfg config.Config) (openai.Client, string) {
	t.Helper()
	cfg = config.WithDefaults(cfg)
	ts := httptest.NewServer(server.New(cfg, nil).Handler())
	t.Cleanup(ts.Close)

	client := openai.NewClient(
		option.WithBaseURL(ts.URL+"/v1"),
		option.WithAPIKey("test"),
		option.WithMaxRetries(0),
	)
	return client, ts.URL
}

var chatParams = openai.ChatCompletionNewParams{
	Model:    "gpt-4o-mini",
	Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
}

func complete(t *testing.T, client openai.Client) string {
	t.Helper()
	completion, err := client.Chat.Completions.New(context.Background(), chatParams)
	if err != nil {
		t.Fatal(err)
	}
	return completion.Choices[0].Message.Content
}

func streamParams() openai.ChatCompletionNewParams {
	params := chatParams
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	return params
}

func accumulateStream(t *testing.T, client openai.Client) openai.ChatCompletionAccumulator {
	t.Helper()
	stream := client.Chat.Completions.NewStreaming(context.Background(), streamParams())
	var acc openai.ChatCompletionAccumulator
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestSequenceReturnsResponsesInOrderThenRepeatsLast(t *testing.T) {
	client, _ := newClient(t, config.Config{OnExhausted: config.OnExhaustedRepeatLast, Responses: []config.Response{
		{Content: "first"},
		{Status: http.StatusTooManyRequests},
		{Content: "third"},
	}})

	if got := complete(t, client); got != "first" {
		t.Fatalf("request 1 content = %q", got)
	}

	_, err := client.Chat.Completions.New(context.Background(), chatParams)
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Message == "" {
		t.Fatalf("request 2 err = %v", err)
	}

	for i := 3; i <= 4; i++ {
		if got := complete(t, client); got != "third" {
			t.Fatalf("request %d content = %q", i, got)
		}
	}
}

func TestExhaustedErrorMode(t *testing.T) {
	client, _ := newClient(t, config.Config{
		OnExhausted: config.OnExhaustedError,
		Responses:   []config.Response{{Content: "only"}},
	})
	complete(t, client)

	_, err := client.Chat.Completions.New(context.Background(), chatParams)
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamReassemblesMultibyteContent(t *testing.T) {
	content := "안녕하세요, 반갑습니다!"
	client, _ := newClient(t, config.Config{Responses: []config.Response{{Content: content}}})

	acc := accumulateStream(t, client)

	choice := acc.Choices[0]
	if choice.Message.Content != content || choice.FinishReason != "stop" {
		t.Fatalf("content=%q finish=%q", choice.Message.Content, choice.FinishReason)
	}
	if acc.Usage.CompletionTokens == 0 {
		t.Fatal("include_usage requested but usage is empty")
	}
}

func TestToolCallStreamAndNonStream(t *testing.T) {
	arguments := `{"city":"Seoul"}`
	client, _ := newClient(t, config.Config{
		Mode:      config.ModeFixed,
		Responses: []config.Response{{Content: "Checking weather", ToolCalls: []config.ToolCall{{Name: "get_weather", Arguments: arguments}}}},
	})

	completion, err := client.Chat.Completions.New(context.Background(), chatParams)
	if err != nil {
		t.Fatal(err)
	}
	choice := completion.Choices[0]
	if choice.Message.Content != "Checking weather" || choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("non-stream choice = %+v", choice)
	}
	if call := choice.Message.ToolCalls[0]; call.Function.Name != "get_weather" || call.Function.Arguments != arguments || call.ID == "" {
		t.Fatalf("non-stream tool call = %+v", call)
	}

	acc := accumulateStream(t, client)
	streamed := acc.Choices[0]
	if streamed.Message.Content != "Checking weather" || streamed.FinishReason != "tool_calls" || len(streamed.Message.ToolCalls) != 1 {
		t.Fatalf("stream choice = %+v", streamed)
	}
	if call := streamed.Message.ToolCalls[0]; call.Function.Name != "get_weather" || call.Function.Arguments != arguments {
		t.Fatalf("stream tool call = %+v", call)
	}
}

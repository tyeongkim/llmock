package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tyeongkim/llmock/internal/config"
	"github.com/tyeongkim/llmock/internal/server"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

const rootHelp = `llmock - mock OpenAI Chat Completions server for E2E tests

Usage:
  llmock <command> [options]

Commands:
  serve      Start the mock server
  validate   Check a config file without starting the server
  version    Print version information
  help       Show help for a command

Run 'llmock help <command>' or 'llmock <command> --help' for details.
`

const serveHelp = `Start a mock server for POST /v1/chat/completions.

Usage:
  llmock serve -c llmock.yaml [options]
  llmock serve response.txt   [options]

With a .txt file, every request is answered with the file contents.
With a config file, responses are served in the order listed in it.
Requests with "stream": true are answered as SSE chunks.

The bound port is printed as the first line of stdout; logs go to stderr.

Endpoints:
  POST /v1/chat/completions   mock completion
  GET  /health                readiness probe (200)
  POST /__admin/reset         restart the response sequence

Options:
`

const validateHelp = `Check a config file and the files it references, without starting the server.

Usage:
  llmock validate -c llmock.yaml

Options:
`

const exampleConfig = `
Example llmock.yaml:
  port: 0                     # 0 picks a free port
  mode: sequence              # sequence | fixed
  on_exhausted: repeat_last   # repeat_last | loop | error
  stream:
    chunk_size: 5
    chunk_delay: 20ms
  responses:
    - file: hello.txt
    - status: 429
    - file: answer.txt
      delay: 500ms
    - tool_call: weather.json
`

var commands = map[string]func(args []string) error{
	"serve":    serve,
	"validate": validate,
}

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("llmock ")
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, rootHelp)
		return 2
	}

	name, rest := args[0], args[1:]
	switch name {
	case "-h", "--help":
		fmt.Print(rootHelp)
		return 0
	case "help":
		return help(rest)
	case "version", "-v", "--version":
		fmt.Printf("llmock %s (commit %s, built %s)\n", Version, Commit, Date)
		return 0
	}

	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "llmock: unknown command %q\n\n%s", name, rootHelp)
		return 2
	}

	err := cmd(rest)
	var usageErr usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &usageErr):
		fmt.Fprintf(os.Stderr, "llmock %s: %v\nRun 'llmock %s --help' for usage.\n", name, usageErr.error, name)
		return 2
	default:
		fmt.Fprintf(os.Stderr, "llmock %s: %v\n", name, err)
		return 1
	}
}

func help(args []string) int {
	if len(args) == 0 {
		fmt.Print(rootHelp)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "llmock: unknown command %q\n\n%s", args[0], rootHelp)
		return 2
	}
	cmd([]string{"--help"})
	return 0
}

type usageError struct{ error }

func newFlagSet(name string) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}

	var configPath string
	flags.StringVar(&configPath, "c", "", "path to llmock.yaml")
	flags.StringVar(&configPath, "config", "", "path to llmock.yaml (same as -c)")
	return flags, &configPath
}

func serve(args []string) error {
	flags, configPath := newFlagSet("serve")
	host := flags.String("host", "", "listen host, overrides config (default 127.0.0.1)")
	port := flags.Int("port", 0, "listen port, 0 picks a free port; overrides config")
	positional, err := parseInterspersed(flags, serveHelp, args)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath, positional)
	if err != nil {
		return err
	}
	if *host != "" {
		cfg.Host = *host
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			cfg.Port = *port
		}
	})

	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	fmt.Println(listener.Addr().(*net.TCPAddr).Port)
	log.Printf("listening on http://%s/v1 with %d response(s)", listener.Addr(), len(cfg.Responses))

	httpServer := &http.Server{Handler: server.New(cfg).Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func validate(args []string) error {
	flags, configPath := newFlagSet("validate")
	positional, err := parseInterspersed(flags, validateHelp, args)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(*configPath, positional)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %d response(s)\n", len(cfg.Responses))
	return nil
}

func printHelp(flags *flag.FlagSet, help string) {
	fmt.Print(help)
	flags.SetOutput(os.Stdout)
	flags.PrintDefaults()
	flags.SetOutput(io.Discard)
	fmt.Print(exampleConfig)
}

func parseInterspersed(flags *flag.FlagSet, help string, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				printHelp(flags, help)
				return nil, err
			}
			return nil, usageError{err}
		}
		if flags.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}
}

func loadConfig(configPath string, positional []string) (config.Config, error) {
	switch {
	case configPath != "" && len(positional) == 0:
		return config.Load(configPath)
	case configPath == "" && len(positional) == 1:
		return config.FromTextFile(positional[0])
	case configPath == "" && len(positional) == 0:
		return config.Config{}, usageError{errors.New("missing config: pass -c llmock.yaml or a response .txt file")}
	default:
		return config.Config{}, usageError{errors.New("pass either -c llmock.yaml or a single response .txt file, not both")}
	}
}

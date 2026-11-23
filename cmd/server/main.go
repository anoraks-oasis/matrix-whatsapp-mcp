// Command server is the matrix-whatsapp-mcp entrypoint. With no arguments it
// loads the configuration, brings up the Matrix sync loop and trigger
// dispatcher, and serves the MCP server over stdio or Streamable HTTP until
// interrupted. The first argument selects one of the helper subcommands
// `chats`, `chats allow|deny` or `doctor`, which run against the homeserver and
// exit instead of serving.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/cli"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/config"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/logger"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/mcpserver"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/trigger"
)

func main() {
	log := logger.New()
	// Any argument means a subcommand: `rooms` and `doctor` are one-shot
	// commands that must not fall through to serving MCP, where an accidental
	// invocation would hang a terminal instead of printing a table.
	if len(os.Args) > 1 {
		deps := cli.DefaultDeps(os.Stdout, os.Stderr)
		os.Exit(cli.Run(context.Background(), os.Args[1:], deps))
	}
	if err := run(log); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// run brings the process up and shuts it down cleanly. It returns nil on a
// graceful shutdown and a non-nil error on a fatal start or sync failure, which
// main turns into a non-zero exit.
func run(log *log.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	matrixClient, err := matrix.New(cfg, log)
	if err != nil {
		return fmt.Errorf("matrix client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	dispatcher := trigger.New(cfg.TriggerWebhook, matrixClient, log)
	dispatcher.Attach(matrixClient)

	var wg sync.WaitGroup

	// Signal watcher: cancel the run on the first signal, then exit.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-sigCh:
			log.Println("shutdown signal received")
			cancel()
		case <-ctx.Done():
		}
	}()

	var fatalMu sync.Mutex
	var fatalErr error
	setFatal := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		cancel()
	}

	// Sync loop. A fatal error (invalid token, user mismatch) cancels the run;
	// a plain cancellation returns nil.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := matrixClient.Sync(ctx); err != nil {
			log.Printf("sync loop stopped: %v", err)
			setFatal(fmt.Errorf("sync loop: %w", err))
		}
	}()

	dispatcher.Start(ctx)

	mcpServer := mcpserver.NewWithAllowlist(matrixClient, matrixClient, dispatcher, matrixClient.Allowlist())

	switch cfg.Transport {
	case config.TransportStdio:
		log.Println("serving MCP over stdio")
		if err := mcpServer.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			setFatal(fmt.Errorf("mcp server: %w", err))
		}
	case config.TransportHTTP:
		if err := serveHTTP(ctx, cfg.HTTPAddr, mcpServer, log); err != nil {
			setFatal(err)
		}
	}

	// Graceful shutdown: the serve loop has already returned. Stop the
	// dispatcher, then the Matrix client, then join every goroutine.
	cancel()

	if err := dispatcher.Close(); err != nil {
		log.Printf("dispatcher close: %v", err)
	}
	if err := matrixClient.Close(); err != nil {
		log.Printf("matrix client close: %v", err)
	}

	wg.Wait()

	fatalMu.Lock()
	defer fatalMu.Unlock()
	return fatalErr
}

// serveHTTP serves the MCP server over Streamable HTTP and blocks until the
// context is cancelled or the listener fails. On cancellation it performs a
// graceful http.Server shutdown, which also closes the MCP sessions.
func serveHTTP(ctx context.Context, addr string, mcpServer *mcp.Server, log *log.Logger) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	// WriteTimeout is deliberately left at 0: the Streamable HTTP transport
	// holds a long-lived GET SSE stream open for server->client messages, and
	// net/http would apply a server-wide write deadline of now+WriteTimeout to
	// every response, cutting that stream. The SDK owns the connection
	// lifecycle; per-request bounds belong in the handler, not here.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("serving MCP over HTTP on %s", addr)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		// Signal or fatal sync error: graceful HTTP shutdown below.
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := srv.Shutdown(shutdownCtx)
	shutdownCancel()
	if err != nil {
		log.Printf("http server shutdown: %v", err)
		_ = srv.Close()
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

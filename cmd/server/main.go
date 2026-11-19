// Command server is the matrix-whatsapp-mcp entrypoint. It loads the
// configuration, brings up the Matrix sync loop and trigger dispatcher, and
// will serve the MCP server (stdio or Streamable HTTP) once Phase B lands.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/logger"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/trigger"

	// Pinned now so go.mod records the resolved version; the MCP server that
	// actually uses this package is wired in Phase B.
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	log := logger.New()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	// cfg.String() redacts secrets. Never log cfg fields directly.

	matrixClient, err := matrix.New(cfg, log)
	if err != nil {
		log.Fatalf("matrix client: %v", err)
	}
	defer func() {
		if err := matrixClient.Close(); err != nil {
			log.Printf("matrix client close: %v", err)
		}
	}()

	dispatcher := trigger.New(cfg.TriggerWebhook, matrixClient, log)
	dispatcher.Attach(matrixClient)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dispatcher.Start(ctx)

	// TODO(Phase B): construct and serve the MCP server here.
	//   - cfg.Transport == config.TransportStdio: JSON-RPC over stdin/stdout.
	//   - cfg.Transport == config.TransportHTTP: Streamable HTTP on cfg.HTTPAddr.
	// The tool handlers must go through matrixClient (MatrixService) and
	// dispatcher.Recent, and enforce cfg.SendAllowed / cfg.ReadOnly.

	syncErr := make(chan error, 1)
	go func() {
		syncErr <- matrixClient.Sync(ctx)
	}()

	select {
	case <-ctx.Done():
		log.Println("shutdown signal received")
	case err := <-syncErr:
		if err != nil {
			log.Printf("sync loop stopped: %v", err)
		}
	}

	if err := dispatcher.Close(); err != nil {
		log.Printf("dispatcher close: %v", err)
	}
}

// Package logger provides the single process-wide logger. It writes to
// os.Stderr only: stdout carries MCP JSON-RPC frames in stdio mode and any
// stray byte there corrupts the protocol.
package logger

import (
	"log"
	"os"
)

// New returns a logger that writes to os.Stderr. Callers must never log
// secrets (access tokens, pickle keys) through it.
func New() *log.Logger {
	return log.New(os.Stderr, "matrix-whatsapp-mcp: ", log.LstdFlags|log.LUTC|log.Lmsgprefix)
}

// Package cli implements the binary's subcommands: `chats`, `chats allow|deny`
// and `doctor`. They exist so enrolling a chat is one command instead of an
// Element lookup, an .env edit and a restart. `rooms` and `rooms allow|deny`
// stay as hidden aliases of the same commands, so existing scripts keep
// working.
//
// Nothing here runs on the server path. With no arguments the binary still
// serves MCP, and that path never writes to stdout.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/allowlist"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/config"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2

	// commandTimeout bounds a whole subcommand, so an unreachable homeserver
	// ends in an error instead of a terminal that never comes back.
	commandTimeout = 30 * time.Second
	// reachabilityTimeout bounds the /versions probe doctor makes.
	reachabilityTimeout = 5 * time.Second

	// probeParallel bounds concurrent chat probes for the WhatsApp check.
	probeParallel = 8
	// probeTimeout bounds the WhatsApp-probe phase of `chats` and `doctor` as a
	// whole. Each chat costs up to two probes with a 10s network timeout each,
	// so an unbounded phase would spend the 30s command budget on a large
	// account and end in a context error with no table at all; cut off, the
	// table still prints and the WhatsApp column says it is partial.
	probeTimeout = 20 * time.Second

	nameWidth = 48
	idWidth   = 80
)

const usage = `matrix-whatsapp-mcp - WhatsApp tooling for a mautrix/whatsapp bridge over Matrix

Usage:
  matrix-whatsapp-mcp                     serve MCP (stdio or HTTP per MCP_TRANSPORT)
  matrix-whatsapp-mcp chats               list joined chats, WhatsApp and send-allowed
  matrix-whatsapp-mcp chats allow <chat>  add a chat to the send allowlist
  matrix-whatsapp-mcp chats deny <chat>   remove a chat from the send allowlist
  matrix-whatsapp-mcp doctor              check the configuration against the homeserver

<chat> is a chat ID, a chat name, or a case-insensitive part of either.
Pass --index N to use the number printed by "chats" instead.

Enrolling a chat:
  1. Invite the bot to the chat from Element.
  2. matrix-whatsapp-mcp chats            # find it in the table
  3. matrix-whatsapp-mcp chats allow 2    # or: chats allow "Family WhatsApp"
`

const chatsUsage = `usage: matrix-whatsapp-mcp chats allow <name-or-chat-id> [--index N]
       matrix-whatsapp-mcp chats deny  <name-or-chat-id> [--index N]`

// RoomService is the part of the Matrix client the subcommands use.
type RoomService interface {
	JoinedRooms(ctx context.Context) ([]matrix.Room, error)
	IsBridged(ctx context.Context, roomID string) bool
	WhoAmI(ctx context.Context) (userID, deviceID string, err error)
	Close() error
}

// Deps are the process edges of a subcommand. Tests replace every one of them,
// so no test needs a homeserver, a terminal or a real environment.
type Deps struct {
	Stdout     io.Writer
	Stderr     io.Writer
	Getenv     func(string) string
	NewClient  func(*config.Config, *log.Logger) (RoomService, error)
	HTTPClient *http.Client
}

// DefaultDeps wires the real process: os.Stdout, os.Stderr, os.Getenv and the
// live Matrix client.
func DefaultDeps(stdout, stderr io.Writer) Deps {
	return Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getenv: os.Getenv,
		NewClient: func(cfg *config.Config, logger *log.Logger) (RoomService, error) {
			return matrix.New(cfg, logger)
		},
	}
}

func (d Deps) withDefaults() Deps {
	if d.Stdout == nil {
		d.Stdout = io.Discard
	}
	if d.Stderr == nil {
		d.Stderr = io.Discard
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.NewClient == nil {
		d.NewClient = DefaultDeps(nil, nil).NewClient
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: reachabilityTimeout}
	}
	return d
}

// Run executes one subcommand and returns the process exit code: 0 on success,
// 1 when a check or command failed, 2 for a usage error. The caller decides
// whether to run a subcommand at all; with no arguments the binary serves MCP
// and never reaches this function.
func Run(ctx context.Context, args []string, deps Deps) int {
	deps = deps.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	if len(args) == 0 {
		fmt.Fprint(deps.Stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "chats", "rooms":
		// rooms is the Matrix-era name for chats and stays a hidden alias so a
		// script written against the old surface keeps working.
		return runChats(ctx, args[1:], deps)
	case "doctor":
		return runDoctor(ctx, args[1:], deps)
	case "help", "-h", "--help":
		fmt.Fprint(deps.Stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(deps.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}

// open loads the configuration and builds the client and the allowlist store.
// The caller closes the returned env.
type cmdEnv struct {
	cfg   *config.Config
	rooms RoomService
	allow *allowlist.Store
	log   *log.Logger
}

func open(deps Deps) (*cmdEnv, error) {
	cfg, err := config.LoadFrom(deps.Getenv)
	if err != nil {
		return nil, err
	}
	rooms, logger, err := buildClient(deps, cfg)
	if err != nil {
		return nil, err
	}
	return &cmdEnv{cfg: cfg, rooms: rooms, allow: newStore(cfg, logger), log: logger}, nil
}

// buildClient constructs the Matrix client with a logger that writes to stderr,
// where every diagnostic of this binary belongs.
func buildClient(deps Deps, cfg *config.Config) (RoomService, *log.Logger, error) {
	logger := log.New(deps.Stderr, "matrix-whatsapp-mcp: ", 0)
	rooms, err := deps.NewClient(cfg, logger)
	if err != nil {
		return nil, logger, err
	}
	return rooms, logger, nil
}

func newStore(cfg *config.Config, logger *log.Logger) *allowlist.Store {
	return allowlist.New(cfg.AllowedSendRooms, cfg.AllowlistFile, logger)
}

func (e *cmdEnv) Close() error { return e.rooms.Close() }

// fail reports err on stderr and returns the failure exit code. Multi-line
// errors, such as an ambiguous room query listing its candidates, keep their own
// shape after the first line so the candidate list stays readable.
func fail(deps Deps, err error) int {
	lines := strings.Split(err.Error(), "\n")
	fmt.Fprintf(deps.Stderr, "error: %s\n", lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintf(deps.Stderr, "%s\n", line)
	}
	return exitFail
}

// probeBridged resolves the WhatsApp flag for every chat, bounded to
// probeParallel concurrent probes and to ctx. It reports whether ctx ended
// before the probes could finish, so the caller can say the column is partial
// instead of presenting an unprobed chat as a confident "not on WhatsApp".
func probeBridged(ctx context.Context, svc RoomService, rooms []matrix.Room) ([]bool, bool) {
	bridged := make([]bool, len(rooms))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeParallel)
	for i := range rooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			bridged[i] = svc.IsBridged(ctx, rooms[i].ID)
		}(i)
	}
	wg.Wait()
	return bridged, ctx.Err() != nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// clamp shortens s to max runes, keeping a prefix so a truncated name is still
// a usable substring query.
func clamp(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// pad right-pads s to width in runes, so a name in a non-Latin script keeps the
// table aligned.
func pad(s string, width int) string {
	if n := len([]rune(s)); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

var errNoAllowlistFile = errors.New("no allowlist file: set MATRIX_ALLOWLIST_FILE, or HOME so the default path can be resolved")

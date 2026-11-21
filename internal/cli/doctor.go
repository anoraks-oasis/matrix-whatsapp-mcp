package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"

	labelWidth = 14
)

// report prints one aligned line per check and counts them. A warn counts as a
// failed check on purpose: the only warning doctor emits is a crypto store the
// server cannot start with.
type report struct {
	w              io.Writer
	passed, failed int
}

func (r *report) check(status, label, detail string) {
	lines := strings.Split(detail, "\n")
	fmt.Fprintf(r.w, "  [%-4s] %s  %s\n", status, pad(label, labelWidth), lines[0])
	indent := strings.Repeat(" ", len("  [")+4+len("] ")+labelWidth+2)
	for _, line := range lines[1:] {
		fmt.Fprintf(r.w, "%s%s\n", indent, line)
	}
	if status == statusOK {
		r.passed++
	} else {
		r.failed++
	}
}

func (r *report) summary() {
	switch {
	case r.passed+r.failed == 0:
		fmt.Fprintln(r.w, "\nno checks ran")
	case r.failed == 0:
		fmt.Fprintf(r.w, "\nall %d checks passed\n", r.passed)
	default:
		fmt.Fprintf(r.w, "\n%d check(s) failed, %d passed\n", r.failed, r.passed)
	}
}

func (r *report) ok() bool { return r.failed == 0 }

func runDoctor(ctx context.Context, args []string, deps Deps) int {
	if len(args) > 0 {
		fmt.Fprintf(deps.Stderr, "doctor takes no arguments\n\n%s\n", usage)
		return exitUsage
	}
	out := deps.Stdout
	fmt.Fprint(out, "matrix-whatsapp-mcp doctor\n\n")

	rep := &report{w: out}
	cfg, err := config.LoadFrom(deps.Getenv)
	if err != nil {
		rep.check(statusFail, "config", err.Error())
		rep.summary()
		return exitFail
	}

	fmt.Fprint(out, "config\n")
	fmt.Fprintf(out, "  %s%s\n", pad("homeserver", labelWidth), cfg.Homeserver)
	fmt.Fprintf(out, "  %s%s\n", pad("user", labelWidth), cfg.UserID)
	fmt.Fprintf(out, "  %s%s\n", pad("transport", labelWidth), transportSummary(cfg))
	fmt.Fprintf(out, "  %s%s\n\n", pad("e2ee", labelWidth), e2eeSummary(cfg))

	fmt.Fprint(out, "checks\n")

	if detail, err := probeVersions(ctx, deps.HTTPClient, cfg.Homeserver); err != nil {
		rep.check(statusFail, "versions", err.Error())
	} else {
		rep.check(statusOK, "versions", detail)
	}

	svc, logger, err := buildClient(deps, cfg)
	if err != nil {
		rep.check(statusFail, "client", err.Error())
		rep.summary()
		return exitFail
	}
	defer svc.Close()

	switch userID, deviceID, err := svc.WhoAmI(ctx); {
	case err != nil:
		rep.check(statusFail, "whoami", err.Error())
	case userID != cfg.UserID:
		rep.check(statusFail, "whoami", fmt.Sprintf("the token belongs to %s, but MATRIX_USER_ID is %s", userID, cfg.UserID))
	default:
		rep.check(statusOK, "whoami", fmt.Sprintf("%s (device %s)", userID, deviceID))
	}

	chats, chatsErr := svc.JoinedRooms(ctx)
	var whatsapp []bool
	if chatsErr != nil {
		rep.check(statusFail, "joined chats", chatsErr.Error())
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		var cut bool
		whatsapp, cut = probeBridged(probeCtx, svc, chats)
		cancel()
		detail := fmt.Sprintf("%d joined, %d on WhatsApp", len(chats), countTrue(whatsapp))
		if cut {
			detail += fmt.Sprintf("\nthe WhatsApp check was cut off after %s, so some chats were not probed", probeTimeout)
		}
		rep.check(statusOK, "joined chats", detail)
	}

	store := newStore(cfg, logger)
	entries := store.Entries()
	if len(entries) == 0 {
		rep.check(statusOK, "send allowlist", "empty, so the server is read-only (fail closed)")
	} else {
		rep.check(statusOK, "send allowlist", allowlistSources(store))
	}

	switch {
	case !cfg.E2EEEnabled:
		rep.check(statusOK, "crypto db", "not needed, MATRIX_E2EE_ENABLED is false")
	default:
		if err := cryptoDBWritable(cfg.CryptoDBPath); err != nil {
			rep.check(statusWarn, "crypto db", err.Error()+"; the server cannot start until this is fixed")
		} else {
			rep.check(statusOK, "crypto db", cfg.CryptoDBPath+" is writable")
		}
	}

	if chatsErr == nil {
		fmt.Fprint(out, "\njoined chats\n")
		printChats(out, chats, whatsapp, store)
	}
	if len(entries) > 0 {
		fmt.Fprint(out, "\nsend allowlist\n")
		for _, entry := range entries {
			fmt.Fprintf(out, "  %s\n      %s\n", entry.RoomID, strings.Join(entry.Sources, ", "))
		}
		fmt.Fprintf(out, "  file %s\n", describeFilePath(store.Path()))
	}

	rep.summary()
	if !rep.ok() {
		return exitFail
	}
	return exitOK
}

func transportSummary(cfg *config.Config) string {
	if cfg.Transport == config.TransportHTTP {
		return fmt.Sprintf("%s on %s", cfg.Transport, cfg.HTTPAddr)
	}
	return string(cfg.Transport)
}

func e2eeSummary(cfg *config.Config) string {
	if !cfg.E2EEEnabled {
		return "disabled"
	}
	return "enabled, crypto db at " + cfg.CryptoDBPath
}

func describeFilePath(path string) string {
	switch {
	case path == "":
		return "no allowlist file path resolved, ALLOWED_SEND_ROOMS only"
	default:
		return describeFile(path)
	}
}

func countTrue(flags []bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// probeVersions checks that the homeserver answers the unauthenticated
// /versions endpoint, which distinguishes "wrong URL" from "bad token".
func probeVersions(ctx context.Context, client *http.Client, homeserver string) (string, error) {
	url := strings.TrimSuffix(homeserver, "/") + "/_matrix/client/versions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build /_matrix/client/versions request: %w", err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /_matrix/client/versions: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read /_matrix/client/versions: %w", err)
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /_matrix/client/versions: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Versions) == 0 {
		return fmt.Sprintf("HTTP 200 in %s", elapsed), nil
	}
	return fmt.Sprintf("HTTP 200 in %s, %d versions, newest %s",
		elapsed, len(parsed.Versions), parsed.Versions[len(parsed.Versions)-1]), nil
}

// cryptoDBWritable reports whether the server could open its SQLite crypto
// store: the file itself when it exists, otherwise a creatable file in its
// directory.
func cryptoDBWritable(path string) error {
	if path == "" {
		return errors.New("MATRIX_CRYPTO_DB_PATH is empty")
	}
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return fmt.Errorf("%s is a directory", path)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return fmt.Errorf("%s is not writable: %v", path, err)
		}
		f.Close()
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".doctor-crypto-*")
	if err != nil {
		return fmt.Errorf("no writable directory %s for the crypto store: %v", dir, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

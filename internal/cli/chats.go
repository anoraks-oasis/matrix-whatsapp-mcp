package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/allowlist"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
)

func runChats(ctx context.Context, args []string, deps Deps) int {
	if len(args) == 0 {
		return listChats(ctx, deps)
	}
	switch args[0] {
	case "allow":
		return enrol(ctx, args[1:], deps, true)
	case "deny":
		return enrol(ctx, args[1:], deps, false)
	default:
		fmt.Fprintf(deps.Stderr, "unknown chats command %q\n\n%s\n", args[0], chatsUsage)
		return exitUsage
	}
}

func listChats(ctx context.Context, deps Deps) int {
	env, err := open(deps)
	if err != nil {
		return fail(deps, err)
	}
	defer env.Close()

	rooms, err := env.rooms.JoinedRooms(ctx)
	if err != nil {
		return fail(deps, fmt.Errorf("listing joined chats: %w", err))
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	bridged, cut := probeBridged(probeCtx, env.rooms, rooms)
	cancel()
	printChats(deps.Stdout, rooms, bridged, env.allow)
	if cut {
		fmt.Fprintf(deps.Stderr, "note: the WhatsApp check was cut off after %s; chats it did not reach are listed as not on WhatsApp\n", probeTimeout)
	}
	return exitOK
}

// printChats renders the table `chats allow --index N` indexes into. The order
// is exactly what JoinedRooms returns (sorted by name), so row numbers stay
// stable between the listing and a later allow/deny.
func printChats(w io.Writer, rooms []matrix.Room, bridged []bool, allow *allowlist.Store) {
	if len(rooms) == 0 {
		fmt.Fprintln(w, "no joined chats")
		fmt.Fprintln(w, "\nInvite the bot to a chat from Element, then run this again.")
		return
	}

	names := make([]string, len(rooms))
	nameW, idW := len("NAME"), len("CHAT ID")
	for i, r := range rooms {
		names[i] = clamp(displayName(r), nameWidth)
		nameW = max(nameW, len([]rune(names[i])))
		idW = max(idW, len([]rune(clamp(r.ID, idWidth))))
	}

	fmt.Fprintf(w, "%3s  %s  %s  %-8s  %s\n", "#", pad("NAME", nameW), pad("CHAT ID", idW), "WHATSAPP", "SEND")
	allowed := 0
	for i, r := range rooms {
		send := allow.Allowed(r.ID)
		if send {
			allowed++
		}
		fmt.Fprintf(w, "%3d  %s  %s  %-8s  %s\n",
			i+1, pad(names[i], nameW), pad(clamp(r.ID, idW), idW), yesNo(bridged[i]), yesNo(send))
	}

	fmt.Fprintf(w, "\n%d chat(s), %d send-allowed\n", len(rooms), allowed)
	fmt.Fprintf(w, "allowlist: %s\n", allowlistSources(allow))
}

// allowlistSources describes the effective allowlist's origins in one line.
func allowlistSources(allow *allowlist.Store) string {
	fromEnv, fromFile := 0, 0
	for _, entry := range allow.Entries() {
		for _, source := range entry.Sources {
			if source == allowlist.EnvSource {
				fromEnv++
			} else {
				fromFile++
			}
		}
	}
	if allow.Path() == "" {
		return fmt.Sprintf("%d from ALLOWED_SEND_ROOMS, no file path resolved", fromEnv)
	}
	return fmt.Sprintf("%d from ALLOWED_SEND_ROOMS, %d from %s", fromEnv, fromFile, describeFile(allow.Path()))
}

func describeFile(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path + " (not created yet)"
	}
	return path
}

func displayName(r matrix.Room) string {
	if r.Name == "" || r.Name == r.ID {
		return "(unnamed)"
	}
	return r.Name
}

func describeChat(r matrix.Room) string {
	if r.Name == "" || r.Name == r.ID {
		return r.ID
	}
	return fmt.Sprintf("%q (%s)", r.Name, r.ID)
}

// selector is one of the two ways to name a chat: a query string, or the row
// number printed by `chats`.
type selector struct {
	query    string
	index    int
	hasIndex bool
}

func parseSelector(args []string) (selector, error) {
	var sel selector
	var words []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--index":
			if i+1 >= len(args) {
				return sel, errors.New("--index needs a value")
			}
			i++
			if err := sel.setIndex(args[i]); err != nil {
				return sel, err
			}
		case strings.HasPrefix(arg, "--index="):
			if err := sel.setIndex(strings.TrimPrefix(arg, "--index=")); err != nil {
				return sel, err
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			return sel, fmt.Errorf("unknown flag %q", arg)
		default:
			words = append(words, arg)
		}
	}
	switch {
	case sel.hasIndex && len(words) > 0:
		return sel, errors.New("give either a chat name or --index, not both")
	case sel.hasIndex:
		return sel, nil
	case len(words) == 0:
		return sel, errors.New("give a chat name, a chat ID, or --index N")
	}
	sel.query = strings.Join(words, " ")
	return sel, nil
}

func (s *selector) setIndex(raw string) error {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return fmt.Errorf("--index must be a positive number, got %q", raw)
	}
	s.index = n
	s.hasIndex = true
	return nil
}

// resolve picks the chat a selector names. An exact chat ID wins outright, then
// an exact case-insensitive name; failing those the query is matched as a
// case-insensitive substring of the name or the chat ID. Anything but a single
// match is an error, and an ambiguous query lists every candidate instead of
// picking one.
func resolve(rooms []matrix.Room, sel selector) (matrix.Room, error) {
	if sel.hasIndex {
		if sel.index < 1 || sel.index > len(rooms) {
			return matrix.Room{}, fmt.Errorf("--index %d is out of range: %d joined chat(s)", sel.index, len(rooms))
		}
		return rooms[sel.index-1], nil
	}

	query := strings.TrimSpace(sel.query)
	for _, r := range rooms {
		if r.ID == query {
			return r, nil
		}
	}
	lower := strings.ToLower(query)
	var exact, partial []matrix.Room
	for _, r := range rooms {
		name := strings.ToLower(r.Name)
		if name == lower {
			exact = append(exact, r)
		}
		if strings.Contains(name, lower) || strings.Contains(strings.ToLower(r.ID), lower) {
			partial = append(partial, r)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return matrix.Room{}, ambiguous(query, exact)
	case len(partial) == 1:
		return partial[0], nil
	case len(partial) > 1:
		return matrix.Room{}, ambiguous(query, partial)
	default:
		return matrix.Room{}, fmt.Errorf("no joined chat matches %q; run `matrix-whatsapp-mcp chats` to list them", query)
	}
}

func ambiguous(query string, candidates []matrix.Room) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d chats, refusing to guess:", query, len(candidates))
	for _, r := range candidates {
		fmt.Fprintf(&b, "\n  %s", describeChat(r))
	}
	b.WriteString("\nname one of them exactly, pass the chat ID, or use --index N")
	return errors.New(b.String())
}

func enrol(ctx context.Context, args []string, deps Deps, allow bool) int {
	sel, err := parseSelector(args)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "%v\n\n%s\n", err, chatsUsage)
		return exitUsage
	}
	env, err := open(deps)
	if err != nil {
		return fail(deps, err)
	}
	defer env.Close()

	rooms, err := env.rooms.JoinedRooms(ctx)
	if err != nil {
		return fail(deps, fmt.Errorf("listing joined chats: %w", err))
	}
	target, err := resolve(rooms, sel)
	if err != nil {
		return fail(deps, err)
	}
	if allow {
		return allowChat(deps, env, target)
	}
	return denyChat(deps, env, target)
}

func allowChat(deps Deps, env *cmdEnv, room matrix.Room) int {
	path := env.cfg.AllowlistFile
	if path == "" {
		return fail(deps, errNoAllowlistFile)
	}
	changed, err := allowlist.Add(path, room.ID)
	if err != nil {
		return fail(deps, fmt.Errorf("writing %s: %w", path, err))
	}

	name := describeChat(room)
	viaEnv := env.cfg.SendAllowed(room.ID)
	switch {
	case !changed && viaEnv:
		fmt.Fprintf(deps.Stdout, "%s is already allow-listed, by ALLOWED_SEND_ROOMS and %s\n", name, path)
	case !changed:
		fmt.Fprintf(deps.Stdout, "%s was already in %s\n", name, path)
	case viaEnv:
		fmt.Fprintf(deps.Stdout, "added %s to %s\nit was already sendable through ALLOWED_SEND_ROOMS\n", name, path)
	default:
		fmt.Fprintf(deps.Stdout, "allowed %s\nwrote %s\nthe running server picks this up within a second; no restart needed\n", name, path)
	}
	return exitOK
}

func denyChat(deps Deps, env *cmdEnv, room matrix.Room) int {
	name := describeChat(room)
	viaEnv := env.cfg.SendAllowed(room.ID)

	removed := false
	path := env.cfg.AllowlistFile
	if path != "" {
		changed, err := allowlist.Remove(path, room.ID)
		if err != nil {
			return fail(deps, fmt.Errorf("writing %s: %w", path, err))
		}
		removed = changed
	}

	switch {
	case removed && viaEnv:
		fmt.Fprintf(deps.Stdout, "removed %s from %s\n", name, path)
		return fail(deps, fmt.Errorf("%s is still allowed by ALLOWED_SEND_ROOMS; remove it there to deny it", room.ID))
	case removed:
		fmt.Fprintf(deps.Stdout, "denied %s\nremoved it from %s\nthe running server picks this up within a second; no restart needed\n", name, path)
		return exitOK
	case viaEnv:
		return fail(deps, fmt.Errorf("%s is allowed by ALLOWED_SEND_ROOMS, which this command does not edit; remove it from the environment to deny it", room.ID))
	default:
		fmt.Fprintf(deps.Stdout, "%s is not in the allowlist; nothing to deny\n", name)
		return exitOK
	}
}

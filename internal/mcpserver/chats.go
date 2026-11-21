package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
)

// maxChatCandidates caps how many chat names an ambiguity or a miss lists, so
// the error stays a correction hint rather than a directory dump.
const maxChatCandidates = 10

// resolveChat turns a chat reference into the chat ID the matrix layer expects.
// chat_id is used as given; chat is resolved against the joined chats by exact
// ID, then exact case-insensitive name, then a unique case-insensitive
// substring of a name. Ambiguity and absence are errors naming candidates, so
// the model can correct itself instead of guessing. A resolved name returns an
// ordinary chat ID: for the write tools it still has to pass the send
// allowlist, because naming a chat never grants permission.
func (s *Server) resolveChat(ctx context.Context, ref chatRef) (string, error) {
	if ref.Chat != "" && ref.chatID() != "" {
		return "", errors.New("pass either chat or chat_id, not both")
	}
	if ref.Chat == "" {
		return ref.chatID(), nil
	}
	query := strings.TrimSpace(ref.Chat)
	if query == "" {
		return "", errors.New("chat must not be empty")
	}

	rooms, err := s.matrix.JoinedRooms(ctx)
	if err != nil {
		return "", err
	}
	if id, candidates, ambiguous := matchChats(rooms, query); id != "" {
		return id, nil
	} else if ambiguous {
		return "", ambiguousChat(query, candidates)
	}

	// list_chats shows a canonical alias, or "unnamed", for a chat whose
	// joined entry carries no display name. Resolve those names and retry once,
	// so any name the caller read from list_chats resolves here too.
	rooms = s.displayChats(ctx, rooms)
	if id, candidates, ambiguous := matchChats(rooms, query); id != "" {
		return id, nil
	} else if ambiguous {
		return "", ambiguousChat(query, candidates)
	}
	if len(rooms) == 0 {
		return "", fmt.Errorf("no chat matches %q: the account has joined no chats", query)
	}
	return "", fmt.Errorf("no chat matches %q; joined chats are: %s", query, chatNames(rooms))
}

// matchChats applies the resolution order to one chat list: exact ID, exact
// case-insensitive name, then a case-insensitive substring of a name. It
// reports the ID on a single match, or the candidates of an ambiguous one.
func matchChats(rooms []matrix.Room, query string) (string, []matrix.Room, bool) {
	for _, r := range rooms {
		if r.ID == query {
			return r.ID, nil, false
		}
	}
	lower := strings.ToLower(query)
	var exact, partial []matrix.Room
	for _, r := range rooms {
		switch {
		case strings.ToLower(r.Name) == lower:
			exact = append(exact, r)
		case strings.Contains(strings.ToLower(r.Name), lower):
			partial = append(partial, r)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0].ID, nil, false
	case len(exact) > 1:
		return "", exact, true
	case len(partial) == 1:
		return partial[0].ID, nil, false
	case len(partial) > 1:
		return "", partial, true
	default:
		return "", nil, false
	}
}

// displayChats returns a copy of rooms with the same fallback names list_chats
// reports: the canonical alias when the joined entry has no name of its own,
// then "unnamed". The copy matters because the joined list is shared state.
func (s *Server) displayChats(ctx context.Context, rooms []matrix.Room) []matrix.Room {
	out := append([]matrix.Room(nil), rooms...)
	if s.prober == nil {
		return out
	}
	probeCtx, cancel := context.WithTimeout(ctx, listChatsTimeout)
	defer cancel()
	sem := make(chan struct{}, listProbeParallel)
	var wg sync.WaitGroup
	for i := range out {
		if out[i].Name != "" && out[i].Name != out[i].ID {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			name := s.prober.CanonicalAlias(probeCtx, out[i].ID)
			if name == "" {
				name = "unnamed"
			}
			out[i].Name = name
		}(i)
	}
	wg.Wait()
	return out
}

// ambiguousChat reports every candidate instead of picking one: two chats named
// "Family" are a question for the caller, not a coin flip.
func ambiguousChat(query string, candidates []matrix.Room) error {
	return fmt.Errorf("chat %q matches %d chats; pass chat_id or a more specific name: %s",
		query, len(candidates), chatNames(candidates))
}

// chatNames lists at most maxChatCandidates names, and says how many it left
// out so the count in the error matches what the caller can see.
func chatNames(rooms []matrix.Room) string {
	limit := min(len(rooms), maxChatCandidates)
	names := make([]string, 0, limit)
	for _, r := range rooms[:limit] {
		names = append(names, r.Name)
	}
	list := strings.Join(names, ", ")
	if len(rooms) > limit {
		list += fmt.Sprintf(", and %d more", len(rooms)-limit)
	}
	return list
}

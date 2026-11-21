package matrix

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

// The kinds of bridge user this package can name. An empty kind is a plain
// Matrix user.
const (
	// BridgeKindContact is a WhatsApp ghost: a WhatsApp contact or group
	// rendered into Matrix by the bridge.
	BridgeKindContact = "whatsapp_contact"
	// BridgeKindBot is the bridge's own bot account.
	BridgeKindBot = "bridge_bot"
)

const (
	// participantTimeout bounds one list_chat_contacts call, including the
	// best-effort profile lookups it makes for members without a display name.
	participantTimeout = 10 * time.Second
	// profileTimeout and profileParallel bound those lookups individually and
	// in flight, so a room of members with no names cannot serialize into a
	// slow call or an unbounded fan-out.
	profileTimeout  = 2 * time.Second
	profileParallel = 8
)

// bridgeMarkerKeys are member-event content keys a bridge uses to mark one of
// its own ghosts. A member carrying one is a ghost even when its localpart does
// not follow the bridge's naming convention.
var bridgeMarkerKeys = []string{"uk.half-shot.bridge", "m.bridge", "fi.mau.bridge"}

// Participant is one joined member of a chat. Bridged and BridgeKind come from
// the same heuristic that list_chats uses for chats (see classifyBridgeUser);
// Membership is the membership the room reports, which is "join" for every
// member of this list.
type Participant struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Membership  string `json:"membership"`
	Bridged     bool   `json:"bridged"`
	BridgeKind  string `json:"bridge_kind"`
}

// Participants implements MatrixService. It returns every joined member of a
// room, sorted by display name and then user ID, with the members the WhatsApp
// bridge owns marked. It is bounded by participantTimeout, and a member whose
// display name is missing from the member list is looked up in the profile API;
// a failed lookup leaves that one name empty rather than failing the list.
func (c *Client) Participants(ctx context.Context, roomID string) ([]Participant, error) {
	if !config.ValidRoomID(roomID) {
		return nil, ErrInvalidRoomID
	}
	callCtx, cancel := context.WithTimeout(ctx, c.participantDeadline)
	defer cancel()

	resp, err := c.cli.Members(callCtx, id.RoomID(roomID), mautrix.ReqMembers{Membership: event.MembershipJoin})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("matrix: list room members: %w", err)
	}

	parts := make([]Participant, 0, len(resp.Chunk))
	seen := make(map[string]bool, len(resp.Chunk))
	for _, evt := range resp.Chunk {
		if evt == nil || evt.Type != event.StateMember {
			continue
		}
		if evt.Content.Parsed == nil {
			if err := evt.Content.ParseRaw(event.StateMember); err != nil {
				continue
			}
		}
		member := evt.Content.AsMember()
		if member == nil || member.Membership != event.MembershipJoin {
			continue
		}
		userID := evt.GetStateKey()
		if userID == "" || seen[userID] {
			continue
		}
		seen[userID] = true
		kind := classifyBridgeUser(userID)
		if kind == "" && memberHasBridgeMarker(evt) {
			kind = BridgeKindContact
		}
		parts = append(parts, Participant{
			UserID:      userID,
			DisplayName: member.Displayname,
			Membership:  string(member.Membership),
			Bridged:     kind != "",
			BridgeKind:  kind,
		})
	}

	// Names the member list did not carry are a best-effort fill, bounded by
	// the call timeout and never fatal: the member still appears.
	c.fillDisplayNames(callCtx, parts)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sort.Slice(parts, func(i, j int) bool {
		a, b := strings.ToLower(parts[i].DisplayName), strings.ToLower(parts[j].DisplayName)
		if a != b {
			return a < b
		}
		return parts[i].UserID < parts[j].UserID
	})
	return parts, nil
}

// fillDisplayNames looks up the profile display name of every participant that
// has none, a bounded number at a time. Any failure is that member's blank name
// and nothing else.
func (c *Client) fillDisplayNames(ctx context.Context, parts []Participant) {
	sem := make(chan struct{}, profileParallel)
	var wg sync.WaitGroup
	for i := range parts {
		if parts[i].DisplayName != "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, profileTimeout)
			defer cancel()
			name, err := c.cli.GetDisplayName(pctx, id.UserID(parts[i].UserID))
			if err != nil || name == nil {
				return
			}
			parts[i].DisplayName = name.DisplayName
		}(i)
	}
	wg.Wait()
}

// classifyBridgeUser is the single bridge-naming heuristic in this package: it
// is what both IsBridged (through the chat's members) and list_chat_contacts use,
// so a chat and its contacts can never disagree about what a WhatsApp ghost is.
// A localpart starting with whatsapp_ (a phone-number ghost) or with
// whatsapp_lid-/whatsapp-lid- (a LID ghost, whichever separator the bridge
// chose) is a contact ghost, a localpart that starts with whatsapp and ends
// with bot is the bridge bot, and any other localpart containing whatsapp stays
// a contact ghost — the looser rule IsBridged has always used, because bridges
// rename ghosts and the phone number is not always the first segment. The
// localpart is lowercased first, so every prefix here is matched
// case-insensitively.
func classifyBridgeUser(userID string) string {
	localpart, _, err := id.UserID(userID).Parse()
	if err != nil {
		return ""
	}
	localpart = strings.ToLower(localpart)
	switch {
	case strings.HasPrefix(localpart, "whatsapp") && strings.HasSuffix(localpart, "bot"):
		return BridgeKindBot
	case strings.HasPrefix(localpart, "whatsapp_"),
		strings.HasPrefix(localpart, "whatsapp_lid-"),
		strings.HasPrefix(localpart, "whatsapp-lid-"),
		strings.Contains(localpart, "whatsapp"):
		return BridgeKindContact
	}
	return ""
}

// memberHasBridgeMarker reports whether a member event carries bridge metadata,
// which marks a ghost whose localpart does not follow the naming convention.
func memberHasBridgeMarker(evt *event.Event) bool {
	if evt == nil {
		return false
	}
	for _, key := range bridgeMarkerKeys {
		if _, ok := evt.Content.Raw[key]; ok {
			return true
		}
	}
	return false
}

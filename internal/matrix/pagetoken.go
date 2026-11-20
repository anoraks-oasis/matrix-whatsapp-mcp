package matrix

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"maunium.net/go/mautrix"
)

// A page token is the bridge between a caller's `before` input and the
// homeserver's /messages cursor. The homeserver cursor is opaque to this
// client but it is not safe to hand back unchanged: on the next call it is
// unvalidated input, it can be replayed against another room, and a typo in it
// makes the homeserver return an empty page instead of an error. A token here
// is the cursor plus a format version and the room it was issued for, signed
// with a per-process key, so a modified, foreign or fabricated token is
// rejected with ErrInvalidPageToken rather than silently paging nowhere.
//
// The token holds no secret — it is the /messages cursor in base64url, and the
// signature only proves this process issued it. Its alphabet is base64url plus
// ".", so it is safe to log. Signing also means a token stops working after a
// restart, which turns a stale token into a clear error; a token the current
// process issued but the homeserver no longer accepts is reported as
// ErrStalePageToken.
const (
	pageTokenVersion = 1
	// pageTokenSigLen truncates the signature to 16 bytes. The key is
	// per-process and the token is not a secret, so this only has to make a
	// guess or an edit impossible, not protect a long-lived credential.
	pageTokenSigLen = 16
	// pageTokenMaxLen bounds the input before anything is decoded, so a caller
	// cannot make the server allocate memory for a multi-megabyte "token".
	pageTokenMaxLen = 1024
	// pageTokenKeyLen is the HMAC-SHA256 key length.
	pageTokenKeyLen = 32
)

// pageTokenPayload is the signed body of a token. The fields are short because
// the token travels in a tool argument on every page request.
type pageTokenPayload struct {
	Version int    `json:"v"`
	RoomID  string `json:"r"`
	Cursor  string `json:"c"`
}

// pageSigner issues and checks page tokens with a key that lives only as long
// as the process.
type pageSigner struct {
	key []byte
}

// newPageSigner returns a signer with a fresh random key. A failure to read
// randomness is reported rather than papered over: an unsigned token would be
// indistinguishable from a token a caller fabricated.
func newPageSigner() (*pageSigner, error) {
	key := make([]byte, pageTokenKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("matrix: generate page token key: %w", err)
	}
	return &pageSigner{key: key}, nil
}

// sign wraps a homeserver cursor for one room. An empty cursor signs to an
// empty token: there is no next page to describe.
func (s *pageSigner) sign(roomID, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	body, err := json.Marshal(pageTokenPayload{Version: pageTokenVersion, RoomID: roomID, Cursor: cursor})
	if err != nil {
		return "", fmt.Errorf("matrix: encode page token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(s.mac(body)), nil
}

// verify returns the homeserver cursor a token carries, or ErrInvalidPageToken
// when the token is not one this process issued for roomID.
func (s *pageSigner) verify(roomID, token string) (string, error) {
	if len(token) > pageTokenMaxLen {
		return "", invalidPageToken("the token is too long")
	}
	encodedBody, encodedSig, ok := strings.Cut(token, ".")
	if !ok {
		return "", invalidPageToken("the token is malformed")
	}
	body, err := base64.RawURLEncoding.DecodeString(encodedBody)
	if err != nil {
		return "", invalidPageToken("the token is malformed")
	}
	sig, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil || !hmac.Equal(sig, s.mac(body)) {
		return "", invalidPageToken("the token was not issued by this server; call again without before, or with the next_token from the last result")
	}
	var payload pageTokenPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", invalidPageToken("the token is malformed")
	}
	if payload.Version != pageTokenVersion {
		return "", invalidPageToken("the token has an unsupported version")
	}
	if payload.RoomID != roomID {
		return "", invalidPageToken("the token was issued for a different room")
	}
	if payload.Cursor == "" {
		return "", invalidPageToken("the token carries no position")
	}
	return payload.Cursor, nil
}

func (s *pageSigner) mac(body []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(body)
	return mac.Sum(nil)[:pageTokenSigLen]
}

// invalidPageToken attaches a fixed explanation to ErrInvalidPageToken. The
// reason is derived from the token's shape only and never echoes the token, so
// a malformed token cannot smuggle text into an error or a log.
func invalidPageToken(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidPageToken, reason)
}

// isStalePageError reports whether a /messages failure means the homeserver
// rejected the from token, as opposed to a network or permission problem. A
// paged request that fails with a bad-parameter error is the homeserver saying
// it no longer knows the position, which is the one failure a caller can fix
// by starting again from the newest message.
func isStalePageError(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.IsStatus(http.StatusBadRequest) {
		return true
	}
	return errors.Is(err, mautrix.MInvalidParam)
}

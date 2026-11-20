package matrix

import (
	"errors"
	"net/http"

	"maunium.net/go/mautrix"
)

// isUnsupportedEndpoint reports whether err is the homeserver saying it does
// not implement an endpoint at all, as opposed to refusing one request about a
// real object. It is what turns "this server has no /search or /context" into a
// fallback instead of a failed tool call.
//
// A 404 carrying M_NOT_FOUND is deliberately not counted: that is a real answer
// about a missing event or room, and reporting an unknown event as "the server
// has no /context" would replace a clear error with an empty result.
func isUnsupportedEndpoint(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mautrix.MUnrecognized) {
		return true
	}
	switch httpStatus(err) {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusNotFound:
		return !errors.Is(err, mautrix.MNotFound)
	}
	return false
}

// httpStatus returns the HTTP status behind a mautrix error, or 0 when the
// error did not reach the homeserver. mautrix returns HTTPError both by value
// and by pointer depending on the code path, so both shapes are checked.
func httpStatus(err error) int {
	var value mautrix.HTTPError
	if errors.As(err, &value) && value.Response != nil {
		return value.Response.StatusCode
	}
	var ptr *mautrix.HTTPError
	if errors.As(err, &ptr) && ptr.Response != nil {
		return ptr.Response.StatusCode
	}
	return 0
}

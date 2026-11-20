#!/usr/bin/env bash
#
# smoke_test.sh — end-to-end smoke checks for matrix-whatsapp-mcp.
#
# It builds the binary with the pure-Go olm backend (the goolm tag), verifies
# the configuration error path, exercises a real stdio JSON-RPC round trip, and
# optionally checks homeserver reachability and the authenticated list_rooms
# path. It never prints the access token and never stores secrets.
#
# Usage:
#   ./scripts/smoke_test.sh                 # build + offline round trip
#   MATRIX_HOMESERVER=https://... MATRIX_USER_ID=@bot:... MATRIX_ACCESS_TOKEN=... \
#       ./scripts/smoke_test.sh             # also run the authenticated checks
#
# Environment overrides:
#   GOOLM_TAG            build tag (default: goolm)
#   SMOKE_STDIO_SETTLE   seconds to hold stdio open for the round trip (default: 2)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GOOLM_TAG="${GOOLM_TAG:-goolm}"
SMOKE_STDIO_SETTLE="${SMOKE_STDIO_SETTLE:-2}"

pass() { printf 'PASS: %s\n' "$1"; }
info() { printf 'INFO: %s\n' "$1"; }
warn() { printf 'WARN: %s\n' "$1" >&2; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
BIN="$WORK/matrix-whatsapp-mcp"

# 1. Build with the goolm tag and assert the artifact is executable.
info "building binary with -tags $GOOLM_TAG"
(cd "$REPO_ROOT" && go build -tags "$GOOLM_TAG" -o "$BIN" ./cmd/server)
[ -f "$BIN" ] || fail "binary was not produced at $BIN"
[ -x "$BIN" ] || fail "binary at $BIN is not executable"
pass "built executable binary with -tags $GOOLM_TAG"

# 2. Missing required variables must produce a non-zero exit and a clear message.
set +e
missing_out="$(env -i "$BIN" 2>&1)"
missing_status=$?
set -e
if [ "$missing_status" -eq 0 ]; then
	fail "binary exited 0 with no configuration"
fi
for var in MATRIX_HOMESERVER MATRIX_USER_ID MATRIX_ACCESS_TOKEN; do
	if ! grep -qF "$var" <<<"$missing_out"; then
		fail "missing-config error did not name $var: $missing_out"
	fi
done
if ! grep -qF "required but not set" <<<"$missing_out"; then
	fail "missing-config error did not say 'required but not set': $missing_out"
fi
pass "binary exits non-zero and names every missing variable"

# 2b. The subcommands must fail the same way without configuration, and must not
#     fall through to serving MCP (which would hang on stdin).
for cmd in rooms doctor; do
	set +e
	sub_out="$(env -i "$BIN" "$cmd" 2>&1)"
	sub_status=$?
	set -e
	if [ "$sub_status" -eq 0 ]; then
		fail "'$cmd' exited 0 with no configuration"
	fi
	for var in MATRIX_HOMESERVER MATRIX_USER_ID MATRIX_ACCESS_TOKEN; do
		if ! grep -qF "$var" <<<"$sub_out"; then
			fail "'$cmd' did not name $var: $sub_out"
		fi
	done
done
pass "rooms and doctor exit non-zero and name every missing variable"

# 3. Read-only homeserver reachability check (skipped without credentials).
if [ -n "${MATRIX_HOMESERVER:-}" ] && [ -n "${MATRIX_USER_ID:-}" ] && [ -n "${MATRIX_ACCESS_TOKEN:-}" ]; then
	info "checking $MATRIX_HOMESERVER/_matrix/client/versions"
	if versions="$(curl -fsS --max-time 10 "${MATRIX_HOMESERVER%/}/_matrix/client/versions" 2>&1)"; then
		pass "homeserver /versions reachable"
		info "versions: $versions"
	else
		warn "homeserver /versions unreachable: $versions"
	fi
else
	info "skipping homeserver reachability check: MATRIX_HOMESERVER/MATRIX_USER_ID/MATRIX_ACCESS_TOKEN not all set"
fi

# JSON-RPC frames for the offline stdio round trip. These carry no secrets.
INIT_JSON='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke_test","version":"1.0"}}}'
INITIALIZED_JSON='{"jsonrpc":"2.0","method":"notifications/initialized"}'
LIST_TOOLS_JSON='{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'

run_stdio_round_trip() {
	# Hold stdin open briefly so the server can emit every response before EOF.
	{
		printf '%s\n' "$INIT_JSON" "$INITIALIZED_JSON" "$LIST_TOOLS_JSON"
		sleep "$SMOKE_STDIO_SETTLE"
	} | env -i PATH="$PATH" \
		MATRIX_HOMESERVER="http://127.0.0.1:9" \
		MATRIX_USER_ID="@smoke:localhost" \
		MATRIX_ACCESS_TOKEN="smoke-test-token" \
		MATRIX_E2EE_ENABLED="false" \
		ALLOWED_SEND_ROOMS="" \
		MCP_TRANSPORT="stdio" \
		"$BIN" 2>/dev/null
}

# 4. Offline stdio round trip: assert all seven tool names come back.
info "running offline stdio JSON-RPC round trip"
stdio_out="$(run_stdio_round_trip)"
for tool in list_rooms read_messages search_messages get_message_context list_participants get_recent_triggers send_message; do
	if ! grep -qF "\"name\":\"$tool\"" <<<"$stdio_out"; then
		fail "tools/list response is missing $tool: $stdio_out"
	fi
done
pass "stdio round trip returned all seven tools"

# 5. Authenticated section: only when a real token is present. Never printed.
if [ -n "${MATRIX_HOMESERVER:-}" ] && [ -n "${MATRIX_USER_ID:-}" ] && [ -n "${MATRIX_ACCESS_TOKEN:-}" ]; then
	info "running authenticated list_rooms through the MCP server"
	LIST_ROOMS_JSON='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_rooms","arguments":{}}}'
	auth_out="$(env -i PATH="$PATH" \
		MATRIX_HOMESERVER="${MATRIX_HOMESERVER}" \
		MATRIX_USER_ID="${MATRIX_USER_ID}" \
		MATRIX_ACCESS_TOKEN="${MATRIX_ACCESS_TOKEN}" \
		MATRIX_E2EE_ENABLED="${MATRIX_E2EE_ENABLED:-false}" \
		ALLOWED_SEND_ROOMS="${ALLOWED_SEND_ROOMS:-}" \
		MCP_TRANSPORT="stdio" \
		"$BIN" 2>/dev/null < <({
			printf '%s\n' "$INIT_JSON" "$INITIALIZED_JSON" "$LIST_TOOLS_JSON" "$LIST_ROOMS_JSON"
			sleep "$SMOKE_STDIO_SETTLE"
		}))"
	if grep -qF '"id":3' <<<"$auth_out"; then
		pass "authenticated list_rooms returned a result"
	else
		warn "authenticated list_rooms produced no id:3 response"
	fi
else
	info "skipping authenticated section: MATRIX_HOMESERVER/MATRIX_USER_ID/MATRIX_ACCESS_TOKEN not all set"
fi

info "smoke test complete"

#!/usr/bin/env bash
# go-adapter smoke suite — runs against a live CLIProxyAPI with the plugin loaded.
#
# What this is: each test below drives the real proxy (http://127.0.0.1:8788) with
# the real plugin in the path, and checks a result you can see: an HTTP status, a
# field in a JSON body, a line in the server log. No mocks. Chat tests use
# hy4-flash-free so the suite costs no paid quota.
#
# How to run (BASE defaults to the example config; the rest has no default):
#   BASE=http://127.0.0.1:8788 CLIENT_KEY=<your access.api-keys entry> \
#   MGMT_KEY=<your management.secret-key> bash scripts/smoke.sh
#
# Optional, enables the two checks that need files rather than HTTP calls:
#   SERVER_LOG=<path to the proxy's stdout log>   # T11 scheduler visibility
#   AUTH_DIR=<the proxy's oauth.auth-dir>         # T12 add-time check
# Without them those checks report SKIP and the summary counts them.
#
# Coverage decisions (feature-smoke-and-guardrails):
#   Restart   — covered by context: the suite runs right after a server restart,
#               and T2/T10 verify auth entries and quota rebuilt from disk.
#   Overload  — skipped: the plugin implements no queues or limits of its own;
#               request size is bounded by the host's max-request-body-bytes.
#   All-or-nothing — skipped: every feature is a single request or a single file
#               write; there are no multi-step writes to leave half-done.

set -u
BASE="${BASE:-http://127.0.0.1:8788}"
CLIENT_KEY="${CLIENT_KEY:-local-dev-key}"
MGMT_KEY="${MGMT_KEY:-test-mgmt-key}"
MODEL="${MODEL:-hy4-flash-free}"
SERVER_LOG="${SERVER_LOG:-}"
AUTH_DIR="${AUTH_DIR:-}"

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); echo "PASS  $1"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL  $1 ${2:+— $2}"; }
skip() { SKIP=$((SKIP+1)); echo "SKIP  $1 ${2:+— $2}"; }

# jget prints one JSON path as JSON (double quotes) so grep stays reliable;
# numeric path segments index into lists.
jget() { python3 -c '
import json,sys
cur=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    if isinstance(cur,list) and k.lstrip("-").isdigit(): cur=cur[int(k)]
    elif isinstance(cur,dict): cur=cur.get(k)
    else: cur=None
    if cur is None: break
print(cur if isinstance(cur,str) else json.dumps(cur))' "$1" 2>/dev/null; }

# T1 — the plugin's static catalogue is served: the free test model and argon
# must both be listed, coming from model.static with zero config providers.
models=$(timeout 30 curl -m 25 -sS "$BASE/v1/models" -H "Authorization: Bearer $CLIENT_KEY" | jget data)
echo "$models" | grep -q "\"$MODEL\"" && ok "T1 catalogue lists $MODEL" || bad "T1 catalogue" "$models"
echo "$models" | grep -q '"gemini-4-argon"' && ok "T1 catalogue lists gemini-4-argon" || bad "T1 catalogue (argon)"

# T2 — add-time validation: the broken key must be listed and disabled, so a bad
# key can never sit silently in the pool; the good key must stay active.
# Fixtures: "go-adapter-1" (a working key) and "go-adapter-broken" (a bogus key).
# They are not created by this script, so a pool without them reports SKIP.
auths=$(timeout 30 curl -m 25 -sS "$BASE/v0/management/auth-files" -H "Authorization: Bearer $MGMT_KEY")
auth_field() { # id field
  printf '%s' "$auths" | python3 -c "
import json,sys
d=json.loads(sys.stdin.read())
m=[f for f in d['files'] if f['id']=='$1']
print(m[0].get('$2','') if m else '')" 2>/dev/null
}
broken=$(auth_field go-adapter-broken disabled)
good=$(auth_field go-adapter-1 disabled)
if [ -z "$broken" ]; then
  skip "T2 add-time validation" "no go-adapter-broken fixture in the pool"
else
  [ "$broken" = "True" ] && ok "T2 broken key is quarantined (disabled)" || bad "T2 broken key not disabled"
fi
if [ -z "$good" ]; then
  skip "T2 good key stays enabled" "no go-adapter-1 fixture in the pool"
else
  [ "$good" = "False" ] && ok "T2 good key stays enabled" || bad "T2 good key disabled"
fi

# T3 — normal use: one chat request through the plugin's executor must return a
# visible answer.
chat() {
  timeout 120 curl -m 110 -sS -o /tmp/smoke_chat.json -w '%{http_code}' "$BASE/v1/chat/completions" \
    -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
    -d "$1"
}
code=$(chat "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: smoke ok\"}],\"max_tokens\":64}")
text=$(jget choices.0.message.content < /tmp/smoke_chat.json)
[ "$code" = "200" ] && [ -n "$text" ] && ok "T3 chat returns an answer ($text)" || bad "T3 chat" "http=$code body=$(head -c 200 /tmp/smoke_chat.json)"

# T4 — streaming: SSE frames must arrive and the stream must terminate
# properly. hy4-flash-free is a reasoning model, so the finish reason may be
# "stop" or "length" (token budget spent on reasoning); the invariant is that
# frames flow, a finish_reason chunk arrives, and [DONE] closes the stream.
timeout 120 curl -m 110 -sS -N "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"count 1 to 3\"}],\"stream\":true,\"max_tokens\":256}" \
  > /tmp/smoke_stream.txt
frames=$(grep -c '^data: ' /tmp/smoke_stream.txt)
if [ "$frames" -ge 2 ] && grep -q '"finish_reason":"\(stop\|length\)"' /tmp/smoke_stream.txt && grep -q '^data: \[DONE\]' /tmp/smoke_stream.txt; then
  reason=$(grep -o '"finish_reason":"[a-z]*"' /tmp/smoke_stream.txt | grep -v null | head -1)
  ok "T4 streaming frames=$frames, terminated ($reason)"
else
  bad "T4 streaming" "frames=$frames $(tail -c 200 /tmp/smoke_stream.txt)"
fi

# T5 — repeats: the same request twice must succeed twice (no double effects to
# undo, but a retried request must not be rejected or duplicated).
c1=$(chat "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say a\"}],\"max_tokens\":32}")
c2=$(chat "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say a\"}],\"max_tokens\":32}")
[ "$c1" = "200" ] && [ "$c2" = "200" ] && ok "T5 repeat request succeeds twice" || bad "T5 repeats" "$c1/$c2"

# T6 — parallel use: two concurrent requests must both succeed.
(chat "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say b\"}],\"max_tokens\":32}" > /tmp/smoke_p1.$$ &
 chat "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say c\"}],\"max_tokens\":32}" > /tmp/smoke_p2.$$ &
 wait)
[ "$(cat /tmp/smoke_p1.$$)" = "200" ] && [ "$(cat /tmp/smoke_p2.$$)" = "200" ] && ok "T6 parallel requests both succeed" || bad "T6 parallel" "$(cat /tmp/smoke_p1.$$)/$(cat /tmp/smoke_p2.$$)"

# T7 — access: a wrong client key must be rejected.
code=$(timeout 30 curl -m 25 -sS -o /tmp/smoke_auth.json -w '%{http_code}' "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer wrong-key" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8}")
[ "$code" != "200" ] && ok "T7 wrong client key rejected (http=$code)" || bad "T7 access" "wrong key accepted"

# T8 — bad input: an unknown model must produce a clear error, and the server
# must still be healthy afterwards.
code=$(timeout 60 curl -m 50 -sS -o /tmp/smoke_badmodel.json -w '%{http_code}' "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}')
body=$(head -c 300 /tmp/smoke_badmodel.json)
echo "$body" | grep -qi "no-such-model\|not found\|no auth\|unavailable" && ok "T8 unknown model rejected clearly (http=$code)" \
  || bad "T8 unknown model" "http=$code $body"
code=$(timeout 30 curl -m 25 -sS -o /dev/null -w '%{http_code}' "$BASE/v1/models" -H "Authorization: Bearer $CLIENT_KEY")
[ "$code" = "200" ] && ok "T8 server healthy after bad model" || bad "T8 health" "http=$code"

# T9 — bad input: malformed JSON must be rejected without side effects.
code=$(timeout 30 curl -m 25 -sS -o /dev/null -w '%{http_code}' "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
  -d '{"model": broken')
[ "$code" != "200" ] && ok "T9 malformed body rejected (http=$code)" || bad "T9 malformed body" "http=$code"

# T10 — quota: the management route must return the three windows plus the score.
idx=$(printf '%s' "$auths" | python3 -c "
import json,sys
d=json.load(sys.stdin)
m=[f for f in d['files'] if f['id']=='go-adapter-1' and not f.get('disabled')]
print(m[0].get('auth_index','') if m else '')" 2>/dev/null)
if [ -z "$idx" ]; then
  skip "T10 quota windows" "no enabled go-adapter-1 fixture in the pool"
else
  code=$(timeout 60 curl -m 50 -sS -o /tmp/smoke_quota.json -w '%{http_code}' \
    "$BASE/v0/management/plugins/go-adapter/quota?auth_index=$idx" -H "Authorization: Bearer $MGMT_KEY")
  buckets=$(jget groups.0.buckets < /tmp/smoke_quota.json | grep -o '"window"' | wc -l)
  score=$(jget summary.0.value < /tmp/smoke_quota.json)
  [ "$code" = "200" ] && [ "$buckets" = "3" ] && [ -n "$score" ] \
    && ok "T10 quota: 3 windows + score=$score" || bad "T10 quota" "http=$code buckets=$buckets $(head -c 200 /tmp/smoke_quota.json)"
fi

# T11 — visibility: a scheduling decision must be visible in the server log.
# The session is logged as a hash (never the raw client header), so this counts
# NEW pick lines and checks the winner, not the session value itself.
sess="smoke-$$"
if [ -z "$SERVER_LOG" ]; then
  skip "T11 scheduler visibility" "SERVER_LOG not set"
else
before=$(grep -ac "go-adapter pick:" "$SERVER_LOG" 2>/dev/null || echo 0)
timeout 120 curl -m 110 -sS -o /tmp/smoke_sched.json -w '%{http_code}' "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' -H "X-Session-ID: $sess" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say d\"}],\"max_tokens\":32}" > /dev/null
after=$(grep -ac "go-adapter pick:" "$SERVER_LOG" 2>/dev/null || echo 0)
line=$(grep -a "go-adapter pick:" "$SERVER_LOG" | tail -1)
[ "$after" -gt "$before" ] && echo "$line" | grep -q 'winner="go-adapter-1"' \
  && ok "T11 scheduler decision logged (session hashed): ${line#*go-adapter pick: }" \
  || bad "T11 scheduler visibility" "${line:-no pick line}"
echo "$line" | grep -q "session=$sess" \
  && bad "T11 raw session leaked into log" || ok "T11 session value not logged raw"
fi

# T12 — add-time check on a fresh add + cleanup: a bogus key file must arrive
# already disabled, and removing it must leave nothing behind.
if [ -z "$AUTH_DIR" ]; then
  skip "T12 add-time check" "AUTH_DIR not set"
else
tmp="$AUTH_DIR/go-adapter-smoketmp.json"
printf '{"type":"go-adapter","api_key":"oc_sk_smoke_bogus_%s","label":"smoketmp"}' "$$" > "$tmp"
found=""
for i in $(seq 1 20); do
  sleep 1
  entry=$(timeout 25 curl -m 20 -sS "$BASE/v0/management/auth-files" -H "Authorization: Bearer $MGMT_KEY" | python3 -c "
import json,sys
m=[f for f in json.load(sys.stdin)['files'] if f['id']=='go-adapter-smoketmp']
print(m[0]['disabled'] if m else '')" 2>/dev/null)
  [ -n "$entry" ] && found="$entry" && break
done
[ "$found" = "True" ] && ok "T12 fresh bogus key arrives disabled" || bad "T12 add-time check" "disabled=${found:-never-listed}"
rm -f "$tmp"
gone=""
for i in $(seq 1 20); do
  sleep 1
  left=$(timeout 25 curl -m 20 -sS "$BASE/v0/management/auth-files" -H "Authorization: Bearer $MGMT_KEY" | python3 -c "
import json,sys
print(len([f for f in json.load(sys.stdin)['files'] if f['id']=='go-adapter-smoketmp']))" 2>/dev/null)
  [ "$left" = "0" ] && gone="yes" && break
done
[ "$gone" = "yes" ] && ok "T12 removed auth leaves no residue" || bad "T12 cleanup" "entry still listed"
fi

# T13 — panel integration: the plugin's status route renders pool state.
code=$(timeout 40 curl -m 30 -sS -o /tmp/smoke_status.json -w '%{http_code}' \
  "$BASE/v0/management/plugins/go-adapter/status" -H "Authorization: Bearer $MGMT_KEY")
accounts=$(jget accounts < /tmp/smoke_status.json | grep -o '"id"' | wc -l)
[ "$code" = "200" ] && [ "$accounts" -ge 1 ] \
  && ok "T13 panel status route lists the pool ($accounts account(s))" \
  || bad "T13 panel status" "http=$code accounts=$accounts $(head -c 200 /tmp/smoke_status.json)"

# T14 — panel integration: the manual refresh re-probes and reports verdicts.
code=$(timeout 60 curl -m 50 -sS -X POST -o /tmp/smoke_refresh.json -w '%{http_code}' \
  "$BASE/v0/management/plugins/go-adapter/refresh" -H "Authorization: Bearer $MGMT_KEY")
checked=$(jget checked < /tmp/smoke_refresh.json)
[ "$code" = "200" ] && [ -n "$checked" ] \
  && ok "T14 manual refresh-all reports verdicts (checked=$checked)" \
  || bad "T14 manual refresh" "http=$code $(head -c 200 /tmp/smoke_refresh.json)"

# T15 — the catalogue is fetched from the gateway, not baked into the binary.
# Guards the "hardcoded model table" defect: /v1/models must agree with the
# catalogue the plugin reports, and that catalogue must say it came from
# upstream (bootstrap = the cold-start fallback was used).
src=$(timeout 40 curl -m 30 -sS "$BASE/v0/management/plugins/go-adapter/status?format=json" \
  -H "Authorization: Bearer $MGMT_KEY" | jget catalogue.source)
listed=$(timeout 40 curl -m 30 -sS "$BASE/v0/management/plugins/go-adapter/status?format=json" \
  -H "Authorization: Bearer $MGMT_KEY" | jget catalogue.models)
served=$(timeout 30 curl -m 25 -sS "$BASE/v1/models" -H "Authorization: Bearer $CLIENT_KEY" \
  | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))' 2>/dev/null)
[ "$src" = "upstream" ] && [ -n "$listed" ] && [ "$listed" = "$served" ] \
  && ok "T15 catalogue served live from upstream ($served models)" \
  || bad "T15 catalogue source" "source=$src listed=$listed served=$served"

echo
echo "smoke result: PASS=$PASS FAIL=$FAIL SKIP=$SKIP (model=$MODEL)"
[ "$FAIL" = "0" ]

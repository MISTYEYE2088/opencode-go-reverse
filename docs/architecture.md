# Architecture

The plugin is one shared library that CLIProxyAPI loads at startup. It owns no
routing, no HTTP server and no credential store of its own: it registers a set of
capabilities, the host calls into them, and everything stateful that the host
already does well (selection, affinity, cooldown, failover, the panel) is left to
the host.

```
panel ─────▶ management API ─────▶ auth entries (one JSON file per key)
client ────▶ /v1/chat/completions ─▶ host scheduler ─▶ executor ─▶ opencode.ai
                                          ▲
                       model catalogue ────┘   quota windows
```

## Capabilities

`plugin.go` registers six capabilities. Each maps to one method family in the RPC
(`sdk/pluginabi`), and each is independently useful.

| Capability | What it does | Source |
| --- | --- | --- |
| `auth_provider` | Turns an `oc_sk_…` file into an auth entry; validates, refreshes, quarantines | `auth.go`, `authstate.go` |
| `executor` | Forwards chat and streaming requests upstream | `executor.go` |
| `model_provider` | Serves the Go catalogue, fetched from the gateway | `models.go` |
| `quota_provider` | Reports the three subscription windows | `quota.go` |
| `scheduler` | Ranks keys per request and picks one | `score.go`, `pick.go`, `scheduler.go` |
| `management_api` | Status API, refresh-all, panel page | `management.go` |

`auth_provider` is not optional for traffic. The host selects an auth entry before
it calls an executor; with no entries, a correctly-served catalogue still fails
every request with `503 auth_not_found`.

## Request path

1. The host resolves the model to this plugin (`model_provider`) and asks
   `scheduler.pick` for a key. The pick may decline — see *Declining is a feature*.
2. The chosen auth entry's secret is handed to the executor in `StorageJSON`.
3. `executor` sends `POST {base-url}/chat/completions` with

   ```
   Authorization: Bearer oc_sk_…
   Content-Type: application/json
   x-opencode-session: <stable id per conversation>
   User-Agent: <configured>
   ```

   The session id is the client's own session header (`X-Session-ID`, sanitised)
   when it sends one; otherwise `sha256(model + first user message)`, 16 bytes —
   32 hex characters. Stability is what makes upstream routing and prompt caching
   work; a random id per request defeats both.
4. Non-streaming responses are buffered and returned. Streaming responses are
   relayed frame by frame through the host (`host.stream.emit` / `host.stream.close`),
   with no re-chunking; a partial line at the end of a read is flushed rather than
   dropped, and every failure is classified (`stream_truncated`,
   `connection_lifecycle`, `transient_transport`) so a truncated stream never looks
   like a completed one.
5. Errors keep their identity. The executor response shape has no status field, so
   failures are returned in the RPC error envelope as `pluginabi.Error`:
   `http_status`, `retry_after_seconds`, `credential_scoped`. That is what lets a
   `429` arrive with its retry hint and lets the host's cooldown logic see a `401`
   as a credential problem rather than a generic 500.

Upstream error mapping (`upstreamError` in `executor.go`):

| Upstream | Mapped to | Effect |
| --- | --- | --- |
| `401`, `403` | `credential_invalid` | key is dropped from the pool |
| `429` | `credential_quota`, `retry_after_seconds` preserved | host cools the key, client sees the retry hint |
| `… isn't included in OpenCode Go` | `model_not_found` | plan gate, no cooldown |
| `5xx` | `transient_transport` | retryable, no credential blame |

Byte caps are deliberate: 64 MiB for a response body, 16 KiB of error text with
512 bytes of context, 64 KiB per stream line. Requests whose declared length
exceeds the C ABI's `MaxInt32` are rejected before any memory is allocated, in
both directions.

## Auth entries

Each key is one file in the host's auth directory (mode `0600`), exactly like an
OAuth credential:

```json
{"type": "go-adapter", "api_key": "oc_sk_…", "label": "go-1"}
```

`auth.parse` runs a **live** quota probe at add time — never a cached verdict, so
a revoked key cannot slip in on a stale result. The verdict decides the entry's
initial state:

| Probe result | State | Why |
| --- | --- | --- |
| `200` + valid usage document | valid | key works now |
| `401`, `403 EntitlementError` | invalid, added **disabled** | a brand-new key that never worked must not enter the pool |
| any other non-200, or a malformed `200` | **suspect** → quarantined | the key answers, but not as a working subscription |
| no HTTP answer at all | unknown, added enabled | a network blip must never punish a good key |

Metadata carries `refresh_interval_seconds: 900`, which is what makes the host
actually schedule `auth.refresh` for a plugin provider, and a short `note` that
the host renders next to the key in the panel — that is where the quarantine state
is visible without opening anything else.

`auth.refresh` **preserves** the host's disabled bit instead of recomputing it, and
does not re-probe quarantined keys (routine checks are off for them, by policy).
Corrupt or keyless storage is refreshed to disabled with reason `storage-corrupt`.

## Model catalogue

`model.static` does not answer from a table compiled into the binary — a frozen
list is the exact defect that made this project necessary (a client with a baked
snapshot cannot see a model added later). The order is:

1. an explicit `models:` config list pins the catalogue; nothing is fetched;
2. otherwise `GET {base-url}/models` with the first usable key (enabled, not
   quarantined), cached for `modelCatalogueTTL` (2 minutes);
3. a failed refresh serves the last good list and logs a warning;
4. only with no successful fetch ever does the bootstrap list apply, and that is
   logged as `catalogue unavailable`.

A response only counts as a catalogue when it is OpenAI-shaped, non-empty, and
was a `200`; duplicates and blank ids are dropped. `/v1/models` reports the result
of this fetch, and the plugin page prints the source (`upstream`, `last-good`,
`bootstrap` or `config`) so "is this list live?" never needs a code read.

## Quota

Two routes serve the same windows. The new one is tried first, the legacy one is a
single fallback, and both failing is a loud failure — never a `0 %`:

| Order | Route | Result |
| --- | --- | --- |
| 1 | `GET https://opencode.ai/inference/go/v1/usage` | `{usage:{rolling,weekly,monthly}}` |
| 2 | `GET https://opencode.ai/zen/go/v1/usage` | byte-identical body today |
| — | anything else | not called |

A response counts as a usage document only if all three windows are present, each
`status ∈ {ok, rate-limited}`, `percent` finite in `0..100`, `rate-limited ⇒ 100 %`,
and `resetsAt` an offset-qualified ISO timestamp. Anything else is retried once on
the legacy route and then reported as unavailable.

Classification is the whole point of the probe: a `401` or a `403 EntitlementError`
is a verdict about the key and quarantines it; `5xx`, `429`, timeouts and malformed
bodies are the network and leave the key exactly as it was.

Reads are cached for 60 s per key and base URL, keyed by `sha256(api_key + base)` so
two keys never share an entry. Concurrent reads for one key share a single
in-flight request (per-key singleflight, so distinct keys still run in parallel);
a failed probe is cached as a failure, so a dead key is not re-probed per request.

Subscription windows are kept separate from upstream throughput `429`s. They have
different causes and different remedies, and merging them into one number is how
you get a scheduler that throttles an account that is merely rate-limited upstream.

## Scheduling

Optional by design: the host already cools, excludes, fails over and recovers. The
scheduler only replaces the host's credential choice when it has a real opinion,
and declines otherwise.

```
score = 700 · rolling_remaining/100          # the 5-hour window dominates
      + 200 · monthly_density                # monthly scored by pace, not raw %
      + 1000 · same_session                  # affinity worth more than any quota gap
      − 10000 · vetoed                       # exhausted window / monthly > 95 %
      (or − 20000 for a quarantined key)
```

`monthly_density` compares spending pace with an even-spend schedule:

```
pace      = (monthly_remaining/100) / ((resetsAt − now) / 30 days)
density   = clamp(pace, 0, 2) / 2          # time fraction floored at 0.02
```

With 20 % of the monthly window left, an account resetting in two days scores the
full 200 while one resetting in 25 days scores 24 — same remaining percentage, and
the first should be spent now because unused quota expires.

Vetoes, in evaluation order: 5-hour exhausted, weekly exhausted, monthly exhausted,
monthly above 95 %. `-free` models are exempt from all four — they draw no paid
quota, so an exhausted month must not close the free channel — while the −20000
quarantine stays model-independent, because a rejected key serves nothing.

Nothing is invented when data is missing: no `resetsAt` falls back to raw
remaining, an unreadable window scores the neutral half-share (450 of 900) rather
than zero, and a reset time in the past beyond an hour is treated as raw
remaining.

### Declining is a feature

`scheduler.pick` returns `handled: false` — "let the host decide" — when:

- the winner's lead is under 1 point (the host's affinity and round-robin are
  better than an arbitrary tie-break);
- every candidate is vetoed (the host produces its normal error or cooldown
  instead of the plugin rejecting traffic);
- the request payload cannot be parsed.

Session affinity is remembered per session hash (sha256 prefix, never the raw
session id) in a map capped at 10 000 entries: expired bindings are dropped first,
then an arbitrary tenth of the map is thinned; a binding only counts while its key
still resolves.

## Quarantine

A key whose quota probe returns a non-transport anomaly is quarantined:

1. it scores −20000, below every veto and immune to the affinity bonus;
2. every routine check stops for it — scheduler, the 15-minute auth refresh, and
   the quota cache spend no further upstream calls on it;
3. the only re-check is manual: the panel's per-key quota button, or
   `POST /v0/management/plugins/go-adapter/refresh` for the whole pool. A valid
   answer re-admits the key immediately.

The state lives in plugin-side scheduler state, never in the host's
`auth.Disabled`. That is a deliberate boundary: the host's merge semantics force
`Disabled` back on once it is set, so a quarantine written there could not be
cleared by a manual check — it would be a one-way door. `auth.Disabled` is
therefore only used where the host owns the decision anyway: add-time rejection of
a key that never worked, and storage-corrupt records.

The state is visible on three surfaces, so debugging never requires reading the
scheduler's mind: the host's auth listing (`note`), the status API (badge, reason
and the −20000 itself), and the plugin page, which renders that API.

Recovery is deliberately manual: a key the gateway rejected is re-checked by the
per-key quota button or the pool-wide refresh, and a valid answer puts it straight
back in the pool.

## Management and panel surface

| Surface | Route |
| --- | --- |
| Status API (JSON) | `GET /v0/management/plugins/go-adapter/status` |
| Manual refresh-all | `POST /v0/management/plugins/go-adapter/refresh` |
| Per-key quota (manual recovery) | `GET`/`POST /v0/management/plugins/go-adapter/quota?auth_index=…` |
| Plugin page (HTML, menu "Go Adapter") | `GET /v0/resource/plugins/go-adapter/status` (`?format=json` for JSON) |

The page is served on the public resource route, which the host does **not**
authenticate, so it contains no pool data at all: it asks for the management key
and fetches the authenticated status route itself, rendering it with DOM nodes and
`textContent` (never `innerHTML`). The refresh button uses the same key.

## Storage and redaction

- Secrets live only in the host's auth files (mode `0600`). Never in
  `config.yaml`, never in logs, never in a management response.
- Logs carry method, path, status, model, tokens and latency. Never bodies, never
  `Authorization`, never `x-opencode-session`.
- Outbound text is scrubbed with `oc_sk_[^\s]+` on the way to a log or an error
  message, and label helpers cut on rune boundaries so a truncated label cannot
  leave half a key.

## Configuration

| Key | Default | Meaning |
| --- | --- | --- |
| `base-url` | `https://opencode.ai/inference/go/openai/v1` | upstream base including path |
| `user-agent` | `opencode-go-reverse/0.1` | sent upstream |
| `models` | empty — catalogue is fetched | YAML list or comma-separated string, pins the list |
| `enabled` | host-managed | injected by the host, accepted so strict decoding passes |
| `priority` | host-managed | injected by the host, accepted and ignored |

Decoding is strict on purpose: a typo such as `baseUrl:` is a config error that
keeps the last known-good config and logs a warning, rather than silently pointing
credentialed traffic at the default upstream. Scoring thresholds are constants
(`defaultVetoMonthlyPercent` 95, `defaultPaceCap` 2.0) rather than config keys, so
a config change cannot silently invert the ranking.

## Build and release

The plugin is a C shared library: `-buildmode=c-shared`, `CGO_ENABLED=1`, and a C
compiler per platform (gcc, mingw-w64). It cannot be cross-built for Windows from
Linux without a cross toolchain, so CI builds each target on its own runner:

```
.github/workflows/build.yml
  test   → go vet, go test -race
  build  → matrix linux/amd64 (gcc), windows/amd64 (msys2 mingw-w64)
  release→ on tag v*, attach both libraries and their SHA256 sums
```

`plugin/VERSION` is the version stamped into the binary via `-X main.pluginVersion`;
the workflow refuses a tag that does not match it. The file name carries the same
version because the host uses `go-adapter-v<version>.<ext>` to pick the newest
binary and to derive the plugin id (`go-adapter`).

The host pin is a normal module requirement (`CLIProxyAPI/v8 v8.0.77`), so a fresh
clone builds with nothing but Go and a C compiler.

## Test map

| File | Subject |
| --- | --- |
| `score_test.go` | formula, weighting, veto precedence, density, deferral |
| `pick_test.go` | pick ranking, ties, all-vetoed, sticky bonus validity |
| `executor_test.go` | session derivation, headers, error mapping, truncation, scrubbing |
| `quota_test.go` | cache keying, singleflight, parallel keys, manual recovery |
| `auth_test.go` | add-time verdicts, refresh semantics, disabled-bit preservation, labels |
| `validate_test.go` | probe classification (invalid / suspect / unknown) |
| `quarantine_test.go` | −20000 policy, routine-check suppression, panel visibility, free models |
| `config_test.go`, `plugin_test.go` | strict decoding, last-known-good config, registration payload |

`scripts/smoke.sh` is the end-to-end suite: it drives a live proxy with the plugin
loaded and asserts statuses, JSON fields and log lines (`T1`–`T15`).

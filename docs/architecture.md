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

This document stays at the level of shape and responsibility. Numbers, routes,
thresholds and edge behaviour live in the code next to the function they belong
to, where they cannot rot: search `#WHY` in the file that owns the behaviour.

## Capabilities

| Capability | What it does | Source |
| --- | --- | --- |
| `auth_provider` | Turns an `oc_sk_…` file into an auth entry; validates, refreshes, quarantines | `auth.go`, `authstate.go` |
| `executor` | Forwards chat and streaming requests upstream | `executor.go` |
| `model_provider` | Serves the Go catalogue, fetched from the gateway | `models.go` |
| `quota_provider` | Reports the subscription windows | `quota.go` |
| `scheduler` | Ranks keys per request and picks one | `score.go`, `pick.go`, `scheduler.go` |
| `management_api` | Status API, refresh-all, panel page | `management.go` |

`auth_provider` is not optional for traffic: the host selects an auth entry
before it calls an executor, so with no entries nothing is servable.

## The three rules of scheduling

The scheduler speaks only for this provider and only with data it actually has:

1. **A model outside this provider is not our business.** We decline before
   scoring anything.
2. **A session keeps its account.** Stickiness outweighs any quota difference;
   when scores tie, the session hash decides and the binding is written, so one
   conversation never rotates between equal keys.
3. **Quota decides only among our own keys, and only until the quota is spent.**
   Another provider's credential is a fallback at zero, not a competitor: any
   usable key of ours outranks it, a spent or quarantined pool falls through to
   it, and a request is served rather than failed.

When the plugin has no opinion it declines (`handled: false`) and the host's own
selection runs — declining is safe and never changes the host's behaviour.

## Failure doctrine

One line runs through the whole plugin: **a definitive verdict about the key is
the only thing allowed to sideline a key.** Everything else — `5xx`, `429`,
timeouts, malformed answers, missing route — is the network, fails open, and
costs the key nothing. Recovery from a definitive rejection is manual on
purpose, because only a human can know the bill was paid.

The same doctrine governs data: a number that could not be read is never
substituted with zero. Unknown windows score neutrally, a missing `resetsAt`
falls back to raw remaining, and a response that is not the shape we expect is
treated as no answer at all rather than a partial one.

## Where the state lives

- **Secrets**: only in the host's auth files. Never in config, never in logs,
  never in a management response.
- **Quota snapshots and quarantine**: plugin-side, keyed by credential, with a
  short TTL — the pick path never waits on the network and never blocks.
- **Session bindings**: plugin-side, hashed, bounded, expiring.
- **The auth id → credential mapping**: cached plugin-side and rewritten
  whenever the host hands us a credential, so a pick does not re-ask the host
  for a mapping that only changes when the host tells us.

## The panel is untrusted

The host does not authenticate resource routes. The plugin's public page
therefore carries no data at all: it asks for the management key and fetches the
authenticated route itself. Everything the user sees on it was already
authenticated by the host.

## Build and release

The plugin is a C shared library (`-buildmode=c-shared`, CGO on). CI builds
linux/amd64 and windows/amd64 on their own runners, tests with `-race` first,
and releases on a `v*` tag that must match `plugin/VERSION` — the single source
of the version number, stamped into the binary and carried in the file name.

`scripts/smoke.sh` is the end-to-end suite against a live proxy; the per-file
unit tests live beside the code they pin.

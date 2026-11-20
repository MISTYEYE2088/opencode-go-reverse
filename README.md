# go-adapter

A CLIProxyAPI plugin that serves an **OpenCode Go** subscription through the
standard OpenAI API, so any client that speaks `/v1/chat/completions` can use the
Go model bundle — `gemini-4-argon` included — without the opencode CLI, an
opencode server, or an OAuth flow.

```
your client ──HTTP──▶ CLIProxyAPI + go-adapter ──HTTPS──▶ opencode.ai/inference/go/openai/v1
                        (auth pool, quota,        Authorization: Bearer oc_sk_…
                         scheduling)             x-opencode-session: <conversation>
```

[![build](https://github.com/MISTYEYE2088/opencode-go-reverse/actions/workflows/build.yml/badge.svg)](https://github.com/MISTYEYE2088/opencode-go-reverse/actions/workflows/build.yml)

## What you get

- **One endpoint for every Go model.** The catalogue is fetched from the gateway,
  per account, and refreshed whenever the host re-registers models (startup,
  config reload, auth change) — never from a table baked into the binary. Chat,
  streaming and non-streaming, all go to the same gateway.
- **Multiple keys in one pool.** Each `oc_sk_…` key is an auth entry added through
  the panel. The host picks per request; the plugin adds quota-aware scheduling on
  top (5-hour window dominates, monthly scored by burn pace, session affinity).
- **Quota you can see.** The 5-hour, weekly and monthly windows are exposed per key
  in the panel and through a management API.
- **Broken keys quarantine themselves.** A key the gateway *definitively rejects*
  (401, or a 403 saying the account has no Go plan) is parked at the bottom of the
  ranking (−20000) and costs no further upstream calls until you re-check it by
  hand; a successful manual check puts it straight back in the pool. A 5xx, a
  timeout or an answer that is not a usage document is treated as the network, not
  the key: the key stays in the pool at a neutral score.
- **Free-tier models keep working when paid quota is spent.** Models with a
  `-free` suffix (`hy4-flash-free`, `beam-free`) draw no paid quota, so window
  exhaustion does not block them.

## Requirements

| Item | Needed |
| --- | --- |
| CLIProxyAPI | **v8.0.77** or compatible; must be a build with plugin support (cgo). The `*_no-plugin` release **cannot** load plugins |
| Linux | the released library needs glibc **2.17** or newer (it is built in manylinux2014) |
| An OpenCode Go seat | an `oc_sk_…` API key from the OpenCode console |
| A client | anything OpenAI-compatible: `curl`, the opencode CLI, SDKs, DSH, … |

## Install

1. Download the library for your platform from
   [Releases](https://github.com/MISTYEYE2088/opencode-go-reverse/releases/latest):

   | Platform | File | Put it in |
   | --- | --- | --- |
   | Linux amd64 | `go-adapter-v<version>.so` | `<cliproxyapi>/plugins/linux/amd64/` |
   | Windows amd64 | `go-adapter-v<version>.dll` | `<cliproxyapi>/plugins/windows/amd64/` |

   The file name decides the plugin id: everything before `-v<version>`.
   `go-adapter-v0.9.1.so` therefore loads as plugin `go-adapter`, and the highest
   version in the directory wins.

2. Enable it in `config.yaml`, and set a management key — without one the host
   registers **no** management route, so the status API, the refresh button and the
   per-key quota buttons do not exist (the plugin page still loads, but it can only
   show its key prompt):

   ```yaml
   management:
     secret-key: "change-me-management-key"   # also your MGMT_KEY for the smoke suite

   plugins:
     enabled: true
     dir: "plugins"
     configs:
       go-adapter:
         enabled: true
         base-url: "https://opencode.ai/inference/go/openai/v1"
         # user-agent is optional; it defaults to identifying this plugin
   ```

3. Start CLIProxyAPI and add your key: use the panel's auth-file dialog (it
   POSTs `/v0/management/auth-files`), or place this file in the auth directory
   yourself:

   ```json
   {"type": "go-adapter", "api_key": "oc_sk_…", "label": "go-1"}
   ```

   The key is validated on the spot: a rejected key is added disabled, a key whose
   quota probe fails in a non-transport way is added quarantined.

4. Check the plugin page (menu **Go Adapter**, or
   `/v0/resource/plugins/go-adapter/status`). It lists every key with its state,
   its quota windows and its scheduler score.

## Use

```bash
curl http://127.0.0.1:8788/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gemini-4-argon","messages":[{"role":"user","content":"hello"}],"max_tokens":64}'
```

Set your client's base URL to `http://127.0.0.1:8788/v1` and use a model id from
`GET /v1/models`.

## Configuration

Everything lives under `plugins.configs.go-adapter` in `config.yaml`:

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Host-managed switch; the plugin is not loaded while false |
| `priority` | host default | Host-managed ordering when several plugins claim a model |
| `base-url` | `https://opencode.ai/inference/go/openai/v1` | Upstream base; includes the path, not just the host |
| `user-agent` | `opencode-go-reverse/0.1` | Sent upstream; identify your client (the shipped example sets `go-adapter/<version>`) |
| `models` | empty — fetched from the gateway | Pin the advertised list instead of fetching: YAML list or comma-separated string |

Decoding is strict: an unknown key (e.g. `baseUrl:`) is a config error in the log
rather than a silent fallback to the default upstream.

The model catalogue always comes from the gateway — never a table baked into the
binary. It is cached briefly, per account and provider-wide; a failed refresh
serves the last good list, and the status page says which source you are looking
at. When a model appears upstream, the proxy publishes it at the host's next
model registration (restart, config save, or auth change) — the plugin's own
view is already current by then.

## Build from source

The plugin is a C shared library, so cgo must be on and a C compiler must be
present (gcc on Linux, mingw-w64 on Windows).

```bash
git clone https://github.com/MISTYEYE2088/opencode-go-reverse
cd opencode-go-reverse/plugin
go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.pluginVersion=$(cat VERSION)" \
  -o go-adapter.so .
```

The first build downloads the three modules (the Go module proxy, or a populated module
cache), and needs a C compiler because the plugin is a C shared library.

That command links against **your** glibc: built on Ubuntu 24.04 the library needs
glibc 2.39 and will not load on RHEL 7/8, Ubuntu 20.04 or Debian 11. The released
Linux library is built against glibc 2.17 for exactly that reason; to reproduce it,
run `scripts/build-linux-container.sh` (docker required), which builds inside
manylinux2014 and prints the resulting baseline.

On Windows the same command with `-o go-adapter.dll` builds against mingw-w64 —
this is what CI does on every push. `plugin/VERSION` is the single source of the
version stamped into the binary; tag `vX.Y.Z` must match it.

Run the tests (no network, no host needed):

```bash
cd plugin && go test ./...
```

Run the end-to-end smoke suite against a running proxy with the plugin loaded:

```bash
BASE=http://127.0.0.1:8788 CLIENT_KEY=local-dev-key MGMT_KEY=test-mgmt-key \
SERVER_LOG=/path/to/server.log AUTH_DIR=/path/to/auth bash scripts/smoke.sh
```

It exercises the real proxy with the real plugin and asserts HTTP statuses, JSON
fields and log lines. Chat tests use a `-free` model by default (`MODEL=` overrides
it). Prerequisites: bash, curl, python3, GNU `timeout`/`seq`.

Two checks expect a pool with two auths, one good (`go-adapter-1`) and one with a
bogus key (`go-adapter-broken`); without those ids they report SKIP instead of
failing. `T11` needs `SERVER_LOG`, `T12` needs `AUTH_DIR`; both are optional.
`scripts/hello.py` talks to the OpenCode Go gateway directly, bypassing the proxy —
useful for telling an upstream problem from a plugin problem.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `503 auth_not_found` | The provider has no auth entry. Add a key; a catalogue alone cannot pay for a request |
| `400 … isn't included in OpenCode Go` | The model is not part of the Go bundle — plan gate, not a plugin bug |
| `403 Model access is disabled` | Org-level model toggle in the OpenCode console |
| `400 MissingSessionID` in the log | Plugin defect: the `x-opencode-session` header was not sent. Report it |
| No models at all | Plugin disabled, in the wrong `plugins/<os>/<arch>/` directory, or the host was built without cgo (`*_no-plugin`) |
| Key listed but never picked | Check the plugin page: a quarantined key shows −20000 and its reason. Re-check it with the quota button or **Refresh all keys** |
| A client lists no `gemini-4-argon` | The client has a baked-in catalogue (pi-ai 0.87.1 ships a frozen list). Point it at `/v1/models` or add the model manually |
| Plugin page says `catalogue: bootstrap` | The gateway could not be reached with any pooled key. Check the key and the log line `catalogue unavailable` |

## Compatibility

- Host: CLIProxyAPI v8.0.77, pinned in `plugin/go.mod` (plugin C ABI 1; the plugin
  registers JSON `schema_version` 10, the host's current contract). Upgrading the
  host means rebuilding the plugin.
- Platforms built by CI: linux/amd64, windows/amd64. macOS builds the same way on
  a Mac (`-buildmode=c-shared` → `.dylib`); CI does not ship it.
- One subscription is fine; the pool exists for several keys, not for sharing one
  seat with other people.

## Documentation

- [docs/architecture.md](docs/architecture.md) — how the plugin is built: request
  path, scoring, quota chain, quarantine, management surface, test map.
- [docs/protocol.md](docs/protocol.md) — the upstream OpenCode Go API: endpoints,
  headers, model gates, error shapes.

## License

MIT — see [LICENSE](LICENSE).

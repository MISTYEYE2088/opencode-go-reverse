# Protocol — OpenCode Go inference API

Everything below was observed on 2026-11-18 against a live Go subscription. Where
a claim comes from a doc page instead of a request, the URL is given.

## Endpoints

| Purpose | URL |
| --- | --- |
| Go models (current) | `https://opencode.ai/inference/go/openai/v1/models` |
| Go chat, OpenAI shape | `https://opencode.ai/inference/go/openai/v1/chat/completions` |
| Go models (legacy alias, still works) | `https://opencode.ai/zen/go/v1/models` |
| Pay-as-you-go (needs credits) | `https://opencode.ai/inference/openai/v1/chat/completions` |
| Anthropic shape | `https://opencode.ai/inference/anthropic/v1/messages` |
| Google native shape | `https://opencode.ai/inference/google/v1beta/models/<model>` |
| Go + Anthropic shape | `https://opencode.ai/inference/go/anthropic/v1` |

Docs: <https://opencode.ai/v2/docs/console/go/> and
<https://opencode.ai/v2/docs/console/inference/>.

`/zen/…` is legacy: the opencode v2.3.0 binary contains `opencode.ai/zen` zero
times, while `@earendil-works/pi-ai@0.87.1` uses `/zen/go/v1` and nothing else.
Both currently return the same model list, so either works — target
`/inference/…`.

## Auth and required headers

```
Authorization: Bearer oc_sk_…
Content-Type: application/json
x-opencode-session: <stable id per conversation>
User-Agent: <your client name>/<version>
```

- The key is the Go API key from the console (`/connect` → OpenCode Go in the
  TUI shows the same string). It is not an OAuth token and there is no token
  exchange step.
- `x-opencode-session` is mandatory on `POST /chat/completions`. Omit it and the response
  is **400**:
  `{"type":"error","error":{"type":"MissingSessionID","message":"Request is missing x-opencode-session and cannot be routed efficiently. …"}}`.
  The docs ask for a *stable* id per conversation (routing + prompt caching), not
  a random one per request.
- The docs ask the client to identify itself with its own user agent rather than
  a generic SDK/HTTP-library name.
- Only one conversation id is needed; nothing else about the session is
  registered up front.

## Request / response

```json
POST /inference/go/openai/v1/chat/completions
{"model":"gemini-4-argon","messages":[{"role":"user","content":"Reply with exactly: hello world"}],"max_tokens":64}
```

```json
{"choices":[{"message":{"content":"hello world"}}],
 "model":"gemini-4-argon",
 "usage":{"prompt_tokens":37,"completion_tokens":6,"total_tokens":43,"cost":0,
          "opencode":{"billing_source":"go","list_cost":0.000134,
                      "allowance":0.000268,
                      "tokens":{"input":37,"output":6,"reasoning":0,
                                "cache_read":0,"cache_write_5m":0,"cache_write_1h":0}}}}
```

- `stream: true` yields ordinary SSE `data: {...}` chunks terminated by
  `data: [DONE]` (4 chunks for a 5-word answer).
- `usage.opencode.billing_source` is `go` when the subscription paid, and the
  response is `cost: 0`; `list_cost`/`allowance` are informational.
- Every response carries `x-opencode-log-id` — quote it when reporting a problem.

## Three independent gates

A model can be in the catalog and still not answer. The gates are separate and
produce different errors, which is why a client should expose them rather than
report "model not found":

| Gate | Where | Symptom |
| --- | --- | --- |
| Plan inclusion | Go plan | `400` `Model gemini-4-carbon isn't included in OpenCode Go.` |
| Org model access | console → Models | `403` `{"type":"permission_error","message":"Model access is disabled. Change model access at https://opencode.ai/console/org_…/models."}` |
| Privacy region / training | console → Settings › Privacy | docs: allowed regions default to **Europe & United States**; a workspace without saved privacy settings also allows Global for requests from China. DeepSeek V4.1 Flash/V4 Pro and LongCat 2.5 Free need **Global**; Muse Spark 1.3/1.2 Contributor need "allow paid endpoints that train on request data" |
| Credits | pay-as-you-go path | `402` `Insufficient balance. … OpenCode Go covers only requests to the OpenCode Go provider.` |

Observed on this account: `glm-5.2`, `kimi-k2.7-code`, `iceman-contributor-free`
→ 403 (org toggle); `hy4` → 200 even though it was absent from the first
`/models` answer; `gemini-4-carbon`/`-hydrogen` → 400 (not in Go).

`GET /models` is therefore *not* an entitlement list. Treat it as a hint and let
a real request decide — or probe with a 1-token request.

## Go model list snapshot (2026-11-18, 14 entries)

`deepseek-v4.1-flash`, `glm-5.3`, `glm-5.3-flash`, `qwen3.8-max`, `qwen4-max`,
`kimi-k3`, `kimi-k3.1-pro`, `grok-4.7`, `minimax-m3.1-flash`,
`muse-spark-1.3-contributor`, `beam-free`, `hy4`, `hy4-flash-free`,
`gemini-4-argon`.

Stable across two calls minutes apart. The catalogue is server-side and changes
(hy4 appeared while pi-ai's frozen list never heard of argon) — never hardcode
it.

## Argon and friends

The docs list a Gemini-4 family: `gemini-4-argon`, `gemini-4-carbon`,
`gemini-4-hydrogen`, plus `gemini-nano-banana-2.1`. Only **argon** is inside Go,
which is consistent with the user report that the official Google API does not
serve it: it is reachable here only because it is model *N* of the Go bundle.

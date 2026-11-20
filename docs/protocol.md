# Protocol — OpenCode Go inference API

What a client of this gateway needs to know, at the level that does not rot:
endpoints, headers, and the shape of failure. Concrete validation rules,
fallback routes and classification thresholds live next to the functions that
apply them (`#WHY` in `validate.go`, `executor.go`, `models.go`).

## Endpoints

Everything is under `https://opencode.ai/inference/go/openai/v1/…` (OpenAI
shape). A legacy route family (`/zen/go/v1/…`) serves the same documents today
and is kept as a fallback only — target the current one. Other shapes
(Anthropic, Google-native) and the pay-as-you-go path exist on the same host and
are documented at <https://opencode.ai/v2/docs/console/go/>.

## Auth and required headers

```
Authorization: Bearer oc_sk_…
Content-Type: application/json
x-opencode-session: <stable id per conversation>
User-Agent: <your client name>/<version>
```

The key is a plain console API key — no OAuth, no token exchange.
`x-opencode-session` is mandatory and must be **stable per conversation**:
it is what upstream routing and prompt cache hit/miss depend on. A random id
per request works but pays cold cache forever; the plugin derives a stable id
from the client's own session header when one is sent.

## Response shape

OpenAI `chat.completions`, plus a usage block whose `opencode` field says who
paid (`billing_source: go` means the subscription, `cost: 0`). Streaming is
ordinary SSE `data:` chunks ending in `data: [DONE]`. Every response carries
`x-opencode-log-id` — quote it when reporting a problem.

## Gates, not a catalogue

A model can appear in `/models` and still refuse to answer. The gates are
independent and produce different errors:

- **Plan inclusion** — `400`, the model is not part of the Go bundle.
- **Org model access** — `403`, a console toggle.
- **Privacy region / training consent** — some models need Global or paid
  training consent in console privacy settings.
- **Credits** — `402`, only on the pay-as-you-go path.

Two consequences that shape the plugin: `/models` is a hint, not an entitlement
list — a capability question is settled by a real request; and the model list is
per subscription and changes, which is why neither this document nor the plugin
records one. Read the live list; never copy it.

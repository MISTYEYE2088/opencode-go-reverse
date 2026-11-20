package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// wire shapes mirror sdk/pluginapi so the host can decode them unchanged.
// []byte fields travel as base64 in JSON on both sides.
type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Headers         map[string][]string `json:"Headers"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	Payload         []byte              `json:"Payload"`
	SourceFormat    string              `json:"SourceFormat"`
	AuthAttributes  map[string]string   `json:"AuthAttributes"`
	StorageJSON     []byte              `json:"StorageJSON"`
	StreamID        string              `json:"stream_id,omitempty"`
}

type executorResponse struct {
	Payload []byte              `json:"Payload"`
	Headers map[string][]string `json:"Headers,omitempty"`
}

type streamChunk struct {
	Payload []byte `json:"Payload"`
}

type streamResponse struct {
	Headers map[string][]string `json:"Headers,omitempty"`
	Chunks  []streamChunk       `json:"Chunks,omitempty"`
}

// streamEmitRequest / streamCloseRequest mirror the host's stream bridge shapes.
type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type streamCloseRequest struct {
	StreamID    string         `json:"stream_id"`
	Error       string         `json:"error,omitempty"`
	ErrorDetail *envelopeError `json:"error_detail,omitempty"`
}

const (
	// responseLimit caps one non-streaming upstream body: generous for a real
	// completion, but a hostile upstream cannot grow host memory past it.
	responseLimit = 64 << 20
	// errorLimit caps how much of a non-2xx body is read at all.
	errorLimit = 16 << 10
	// lineBuf bounds how much of a single newline-free upstream line may sit
	// in memory before it is flushed mid-line: the host deframer reassembles
	// events at arbitrary chunk boundaries, so splitting is safe.
	lineBuf = 64 << 10
	// errorContext is how much of an upstream error body may reach a client.
	errorContext = 512
	// requestLimitDefault caps a request body before it is copied again: the
	// host's own max-request-body-bytes defaults to unlimited, and each client
	// byte costs several inside the plugin.
	requestLimitDefault = 32 << 20
	// upstreamTimeout is the generous absolute cap on one upstream call; the
	// header wait is bounded separately so long legitimate generations are not
	// killed mid-stream while a black-holing gateway still cannot pin a caller.
	// No cancellation: the C ABI has no cancel call, so a client that
	// disconnects mid-request still pays for the generation it started. This
	// timeout is the only bound the plugin can apply.
	upstreamTimeout = 10 * time.Minute
	headerTimeout   = 30 * time.Second
)

// secretPattern masks key material that a diagnostic body might echo back.
// secretPattern redacts the whole non-whitespace run after the key prefix: a
// charset-specific pattern would leak every character past the first one
// outside the class.
// The run is unbounded: a quantifier window is padding-bypassable, and a
// character-class regex without backtracking stays linear regardless.
var secretPattern = regexp.MustCompile(`oc_sk_[^\s]+`)

// sseBlank matches a blank line in LF or CRLF form.
var sseBlank = regexp.MustCompile(`\r?\n\r?\n`)

// responseCap is the per-stream/per-response upstream byte cap. A variable so
// the truncation paths are testable without serving 64 MiB.
var responseCap int64 = responseLimit

// requestCap is the per-request body cap; a variable so the rejection path is
// testable without a 32 MiB fixture.
// envelopeLimit bounds the whole JSON envelope the host hands us (payload is
// base64 inside it) before we decode anything. It cannot prevent the ABI's own
// copy: the host hands the plugin a byte slice and C.GoBytes has already copied
// it by the time Go sees the request, so this caps decoding, not allocation.
const envelopeLimit = 64 << 20

var requestCap int64 = requestLimitDefault

// lineBufSize bounds one buffered read of a newline-free line; a variable so the
// mid-line flush path is testable without a 64 KiB payload.
var lineBufSize = lineBuf

// scrub truncates and masks a body fragment destined for a client-visible message.
//
// #FUNC     Truncate at a rune boundary, then mask key-shaped runs.
// #WHY      the cut happens before masking on purpose — a partially-read
//
//	`oc_sk_…` prefix still matches the pattern and is redacted — and at
//	a rune boundary so a hostile multi-byte body cannot push invalid
//	UTF-8 into a client-visible message.
func scrub(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	text = secretPattern.ReplaceAllString(text, "oc_sk_[redacted]")
	// One line, always: the host writes this verbatim into server.log, and a
	// newline in an upstream body would let it forge log records.
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
}

// sessionFor derives the per-conversation routing id the gateway requires.
// The scheduler hook receives no body, so the same derivation also serves as the
// scheduler's session key: model plus the first user turn, hashed.
func sessionFor(model string, payload []byte) string {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	first := ""
	if json.Unmarshal(payload, &body) == nil {
		for _, m := range body.Messages {
			if m.Role != "user" {
				continue
			}
			switch v := m.Content.(type) {
			case string:
				first = v
			case []any:
				var sb strings.Builder
				for _, part := range v {
					if obj, ok := part.(map[string]any); ok {
						if t, okText := obj["text"].(string); okText {
							sb.WriteString(t)
						}
					}
				}
				first = sb.String()
			}
			break
		}
	}
	sum := sha256.Sum256([]byte(model + "\x00" + first))
	return hex.EncodeToString(sum[:16])
}

// keyFor resolves the credential for this request from the auth record the host
// selected for this conversation.
//
// #FUNC     Read the selected auth's credential.
// #NOT      There is deliberately no plugin-config fallback.
// #EDGE-OUT A corrupt or empty auth record errors loudly instead of silently
//
//	billing another key → TestExecuteRequiresAuthCredential
//
// #WHY      `cfg.APIKey` fallback would charge requests to the wrong account the
//
//	moment an auth file is corrupted — misattribution is worse than an error.
func keyFor(req executorRequest) (string, error) {
	if len(req.StorageJSON) > 0 {
		var cred credential
		if json.Unmarshal(req.StorageJSON, &cred) == nil {
			if key := strings.TrimSpace(cred.APIKey); key != "" {
				return key, nil
			}
		}
	}
	return "", fmt.Errorf("no credential on the selected auth record (%s)", req.AuthID)
}

// chatURL is the OpenAI-compatible chat endpoint for the configured base.
func (c pluginConfig) chatURL() string { return c.BaseURL + "/chat/completions" }

// openChat performs the upstream POST and returns the raw response with the body
// still open; the caller decides how to consume it.
func openChat(apiKey string, payload []byte, model string, headers map[string][]string) (*http.Response, error) {
	cfg := currentConfig()
	req, errReq := http.NewRequest(http.MethodPost, cfg.chatURL(), bytes.NewReader(payload))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("x-opencode-session", upstreamSession(headers, model, payload))
	return upstreamClient().Do(req)
}

// upstreamSession picks the conversation id sent upstream: the client's own
// session header when it supplied one, else the derived hash. Both are stable per
// conversation, which is what upstream routing and prompt caching need.
//
// #FUNC     Choose and sanitise the x-opencode-session value.
// #EDGE-IN  no header, a blank header, control characters, or an over-long value
//
//	→ that input is ignored and the derived hash is used instead
//
// #WHY      a client-controlled header goes on the wire; unsanitised it could
//
//	inject headers or smuggle log-shaped text upstream.
func upstreamSession(headers map[string][]string, model string, payload []byte) string {
	if raw := sessionKeyFromHeaders(headers); raw != "" {
		clean := strings.Map(func(r rune) rune {
			// A conservative id alphabet: no spaces, no CR/LF, nothing that
			// changes the shape of the header line.
			if r == '-' || r == '_' || r == '.' || r == ':' {
				return r
			}
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return -1
		}, raw)
		if clean != "" && len(clean) <= 128 {
			return clean
		}
	}
	return sessionFor(model, payload)
}

// upstreamClient waits at most headerTimeout for response headers, then lets
// the body run to the absolute upstreamTimeout.
//
// #WHY      the transport is a `Clone` of `http.DefaultTransport`, not a bare
//
//	struct: the default carries the proxy environment this network
//	needs for egress, and a bare one silently blackholes every call.
func upstreamClient() *http.Client { return upstreamHTTPClient(upstreamTimeout) }

// upstreamHTTPClient builds a client sharing one transport, so connections are
// reused: a fresh transport per call paid a TCP+TLS handshake every request and
// kept dead sockets around until the idle timeout.
func upstreamHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: upstreamTransport()}
}

var (
	transportOnce sync.Once
	transport     *http.Transport
)

// upstreamTransport clones the default transport once. It must be a Clone: the
// default carries the proxy environment this network needs for egress.
// #FUNC      Build the shared upstream HTTP transport.
// #NOT       Not a fresh client per call.
// #WHY       `DefaultTransport.Clone` keeps proxy and TLS defaults while owning
//
//	this pool's connection limits: a fresh transport per request cost
//	one TCP+TLS handshake per call on the same host.
func upstreamTransport() *http.Transport {
	transportOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = headerTimeout
		t.MaxIdleConnsPerHost = 32
		transport = t
	})
	return transport
}

// transportError turns a dial/TLS/reset failure into the error code the host
// treats as transient, so a network blip does not cool the model for 60 s the way
// an untyped executor_error would.
// #FUNC      Classify a transport-level failure.
// #OUT       `transient_transport`: a dead connection is nobody's fault.
// #WHY       labelling a transport failure `request_scoped` makes the host blame
//
//	the caller; `credential_invalid` would drop a good key.
func transportError(err error) *envelopeError {
	return &envelopeError{
		Code:    "transient_transport",
		Message: "upstream unreachable: " + scrub([]byte(err.Error()), errorContext),
	}
}

// requestError is a client-caused failure: it must not cool the credential.
func requestError(status int, message string) *envelopeError {
	return &envelopeError{Code: "request_scoped", Message: message, HTTPStatus: status}
}

// retryAfterOf reads the upstream Retry-After hint, if any.
// #FUNC      Read the upstream retry hint from `Retry-After`.
// #WHY       the hint is honoured verbatim so the host's cooldown uses the
//
//	gateway's own number, not a guess.
func retryAfterOf(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	if secs, errParse := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); errParse == nil && secs > 0 && secs < 86400 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// upstreamError turns a non-2xx answer into the structured error the host's
// cooldown classifier and the client both understand: the status survives, the
// retry hint survives, credential-wide failures are marked as such, and the
// embedded body is truncated and scrubbed.
// #FUNC      Map one upstream failure onto the host's error taxonomy.
// #EDGE-IN   `401`/`403` → `credential_invalid`: the host drops the key
//
//	→ TestUpstreamErrorScrubbedAndStructured
//
// #EDGE-IN   `429` → `credential_quota` with `retry_after` preserved
//
//	→ TestStreamUpstreamErrorCarriesRetryAfter
//
// #EDGE-IN   body containing "isn't included in OpenCode Go" → `model_not_found`:
//
//	the plan gate is neither a credential nor a transport problem
//
// #EDGE-IN   `5xx` → `transient_transport`: retryable, no credential blame
// #WHY       the RPC envelope has no status field, so this classification IS the
//
//	error's identity downstream.
func upstreamError(status int, body []byte, retryAfter time.Duration) *envelopeError {
	code, scoped := "upstream_error", false
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code, scoped = "credential_invalid", true
	case status == http.StatusTooManyRequests:
		code, scoped = "credential_quota", true
	case status == http.StatusBadRequest && strings.Contains(string(body), "isn't included in OpenCode Go"):
		code = "model_not_found"
	case status == http.StatusBadRequest, status == http.StatusRequestEntityTooLarge,
		status == http.StatusUnprocessableEntity:
		// The client sent something the gateway refused: cooling the shared
		// credential for it would punish every other client.
		code = "request_scoped"
	case status >= http.StatusInternalServerError:
		code = "transient_transport"
	}
	return &envelopeError{
		Code:              code,
		Message:           fmt.Sprintf("upstream %d: %s", status, scrub(body, errorContext)),
		HTTPStatus:        status,
		RetryAfterSeconds: int(retryAfter.Seconds()),
		CredentialScoped:  scoped,
	}
}

// readErrorBody reads a capped slice of a non-2xx body for the error message.
func readErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorLimit))
	return body
}

// handleExecute runs the non-streaming path.
func handleExecute(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor request: %w", err)
	}
	apiKey, errKey := keyFor(req)
	if errKey != nil {
		return nil, errKey
	}
	if int64(len(req.Payload)) > requestCap {
		return nil, requestError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds the %d byte cap", requestCap))
	}
	resp, errDo := openChat(apiKey, req.Payload, req.Model, req.Headers)
	if errDo != nil {
		return nil, transportError(errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamError(resp.StatusCode, readErrorBody(resp), retryAfterOf(resp.Header))
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
	if errRead != nil {
		return nil, transportError(errRead)
	}
	if int64(len(body)) > responseCap {
		return nil, &envelopeError{
			Code:    "stream_truncated",
			Message: fmt.Sprintf("upstream response exceeded the %d byte cap", responseCap),
		}
	}
	return json.Marshal(executorResponse{Payload: body, Headers: pickHeaders(resp.Header)})
}

// handleExecuteStream runs the streaming path.
//
// #FUNC     Stream one chat completion, incrementally when the host opened a stream bridge.
// #NOT      Never buffers a whole stream when a StreamID is present.
// #EDGE-IN  A host without a stream bridge (no StreamID) gets the buffered fallback → TestStreamFallbackFramesPreserveBytes
// #EDGE-OUT A non-2xx upstream becomes a structured error before any chunk is emitted → TestStreamUpstreamErrorCarriesRetryAfter
// #WHY      chunks are emitted through `host.stream.emit` as they are read: the
//
//	buffered-array shape pins a host goroutine and ~1.5x the stream size
//	in memory for the whole generation, which is a DoS handle, not streaming.
func handleExecuteStream(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode executor request: %w", err)
	}
	apiKey, errKey := keyFor(req)
	if errKey != nil {
		return nil, errKey
	}
	if int64(len(req.Payload)) > requestCap {
		return nil, requestError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds the %d byte cap", requestCap))
	}
	resp, errDo := openChat(apiKey, req.Payload, req.Model, req.Headers)
	if errDo != nil {
		return nil, transportError(errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamError(resp.StatusCode, readErrorBody(resp), retryAfterOf(resp.Header))
	}
	if req.StreamID != "" {
		go pumpStream(req.StreamID, resp)
		return json.Marshal(streamResponse{Headers: pickHeaders(resp.Header)})
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
	if errRead != nil {
		return nil, transportError(errRead)
	}
	if int64(len(body)) > responseCap {
		return nil, &envelopeError{
			Code:    "stream_truncated",
			Message: fmt.Sprintf("upstream response exceeded the %d byte cap", responseCap),
		}
	}
	out := streamResponse{Headers: pickHeaders(resp.Header)}
	for _, frame := range splitSSE(body) {
		out.Chunks = append(out.Chunks, streamChunk{Payload: frame})
	}
	return json.Marshal(out)
}

// pumpStream forwards upstream bytes to the host stream bridge frame by frame,
// then closes the stream with a code the host's cooldown logic understands.
func pumpStream(streamID string, resp *http.Response) {
	closeErr := classifyStreamError(relayFrames(streamID, resp.Body, emitViaHost(streamID)))
	_ = resp.Body.Close()
	_ = callHost(pluginabi.MethodHostStreamClose, streamCloseRequest{
		StreamID:    streamID,
		ErrorDetail: closeErr,
	}, nil)
}

// emitViaHost adapts the host callback into the emitter relayFrames expects.
func emitViaHost(streamID string) func([]byte) error {
	return func(payload []byte) error {
		return callHost(pluginabi.MethodHostStreamEmit, streamEmitRequest{
			StreamID: streamID,
			Payload:  payload,
		}, nil)
	}
}

// classifyStreamError maps a relay outcome onto the host's recognized codes:
// truncation and transport blips must not cool an otherwise healthy credential.
//
// #FUNC     Map relay failures to host-recognized stream codes.
// #WHY      an unrecognized code like `stream_error` falls into the host's
//
//	default cooldown path, so every mid-stream TCP blip would cool a
//	healthy account; `transient_transport` and `stream_truncated` skip it.
func classifyStreamError(err error) *envelopeError {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errStreamTruncated):
		return &envelopeError{Code: "stream_truncated",
			Message: fmt.Sprintf("upstream stream exceeded the %d byte cap", responseCap)}
	case errors.Is(err, errEmitClosed):
		return &envelopeError{Code: "connection_lifecycle",
			Message: "downstream stream closed while relaying"}
	default:
		return &envelopeError{Code: "transient_transport",
			Message: scrub([]byte(err.Error()), errorContext)}
	}
}

// errStreamTruncated flags a body cut at the response cap.
var errStreamTruncated = errors.New("stream exceeded cap")

// errEmitClosed flags that the host bridge refused an emit (client gone).
var errEmitClosed = errors.New("stream emit refused by host")

// relayFrames reads SSE events (LF or CRLF framed) and emits each one raw.
// A single newline-free line is flushed every lineBuf bytes, so no line —
// however hostile — can be buffered whole.
// #FUNC      Relay one SSE stream frame-by-frame through the host.
// #NOT       No re-chunking: frames are emitted as they arrive.
// #EDGE-OUT  a partial line at stream end is flushed, not dropped →
//
//	TestRelayHugeLineFlushesMidLine
//
// #WHY       buffering the whole body would hold the host request open with no
//
//	output for the whole upstream duration.
func relayFrames(streamID string, body io.Reader, emit func([]byte) error) error {
	reader := bufio.NewReaderSize(io.LimitReader(body, responseCap+1), lineBufSize)
	var total int64
	var frame []byte
	flush := func() error {
		if len(frame) == 0 {
			return nil
		}
		if errEmit := emit(frame); errEmit != nil {
			return errEmitClosed
		}
		frame = frame[:0]
		return nil
	}
	for {
		chunk, errRead := reader.ReadSlice('\n')
		frame = append(frame, chunk...)
		total += int64(len(chunk))
		if total > responseCap {
			return errStreamTruncated
		}
		if errRead == bufio.ErrBufferFull {
			// Line longer than the buffer: flush what we already hold.
			if errFlush := flush(); errFlush != nil {
				return errFlush
			}
			continue
		}
		if len(frame) > lineBufSize {
			// Event-level bound: an upstream that never sends a blank line
			// must not accumulate an unbounded event in memory.
			if errFlush := flush(); errFlush != nil {
				return errFlush
			}
		}
		if strings.TrimRight(string(chunk), "\r\n") == "" && len(frame) > 0 {
			if errFlush := flush(); errFlush != nil {
				return errFlush
			}
		}
		if errRead != nil {
			if errRead == io.EOF {
				_ = flush()
				return nil
			}
			return errRead
		}
	}
}

// splitSSE cuts a buffered SSE body into whole events for the fallback path.
// It splits on a blank line in either LF or CRLF form and keeps each event's
// original bytes, delimiter included.
// #FUNC      Split a stream buffer into complete SSE lines.
// #EDGE-OUT  an incomplete trailing line → kept as the remainder for the next
//
//	read, never emitted half → TestRelayHugeLineFlushesMidLine
//
// #WHY       emitting a partial JSON frame hands the client a parse error that
//
//	looks like an upstream failure.
func splitSSE(body []byte) [][]byte {
	locs := sseBlank.FindAllIndex(body, -1)
	frames := make([][]byte, 0, len(locs)+1)
	start := 0
	for _, loc := range locs {
		if loc[1] > start {
			frames = append(frames, body[start:loc[1]])
		}
		start = loc[1]
	}
	if start < len(body) && len(strings.TrimRight(string(body[start:]), "\r\n")) > 0 {
		frames = append(frames, body[start:])
	}
	return frames
}

// pickHeaders copies the headers worth forwarding downstream.
func pickHeaders(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	out := map[string][]string{}
	for _, k := range []string{"Content-Type", "X-Opencode-Log-Id", "Retry-After"} {
		if v := h.Values(k); len(v) > 0 {
			out[k] = v
		}
	}
	return out
}

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// inputs-2-2 / secrets-2-3: hitting the response cap is flagged, never silent.
func TestStreamTruncationFlagged(t *testing.T) {
	old := responseCap
	responseCap = 256
	t.Cleanup(func() { responseCap = old })

	var emitted [][]byte
	err := relayFrames("s", strings.NewReader("data: "+strings.Repeat("x", 1024)+"\n\n"), func(p []byte) error {
		emitted = append(emitted, append([]byte(nil), p...))
		return nil
	})
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("cap hit must flag truncation, got %v", err)
	}
}

// inputs-2-1: a newline-free line is flushed mid-line, never buffered whole.
func TestRelayHugeLineFlushesMidLine(t *testing.T) {
	old := lineBufSize
	lineBufSize = 512
	t.Cleanup(func() { lineBufSize = old })

	var frames [][]byte
	err := relayFrames("s", strings.NewReader(strings.Repeat("x", 4096)+"\n\n"), func(p []byte) error {
		frames = append(frames, append([]byte(nil), p...))
		return nil
	})
	if err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if len(frames) < 4 {
		t.Fatalf("a 4KiB line over a 512B buffer must flush repeatedly, got %d frame(s)", len(frames))
	}
	for _, f := range frames {
		if len(f) > 1024 {
			t.Fatalf("no buffered frame may exceed the line buffer meaningfully: %d", len(f))
		}
	}
}

// sched-2-4: relay failures map to host-recognized, no-cooldown codes.
func TestClassifyStreamError(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{errStreamTruncated, "stream_truncated"},
		{errEmitClosed, "connection_lifecycle"},
		{errors.New("read: connection reset"), "transient_transport"},
	}
	for _, c := range cases {
		detail := classifyStreamError(c.err)
		if detail == nil || detail.Code != c.code {
			t.Fatalf("classify(%v) = %+v, want code %q", c.err, detail, c.code)
		}
	}
	if classifyStreamError(nil) != nil {
		t.Fatal("a clean relay must close without an error detail")
	}
}

// inputs-2-2, non-stream half: an over-cap response errors instead of lying.
func TestNonStreamTruncationIsStructured(t *testing.T) {
	old := responseCap
	responseCap = 64
	t.Cleanup(func() { responseCap = old })

	srv := chatServer(t, 200, strings.Repeat("y", 512), "")
	setBase(t, srv.URL)

	_, err := handleExecute(execReq(`{"api_key":"k"}`, goodPayload))
	var structured *envelopeError
	if !errors.As(err, &structured) || structured.Code != "stream_truncated" {
		t.Fatalf("over-cap response must error as stream_truncated, got %v", err)
	}
}

// secrets-2-4: scrub never emits invalid UTF-8 from a multi-byte cut.
func TestScrubRuneBoundary(t *testing.T) {
	out := scrub([]byte(strings.Repeat("€", 600)), 512)
	if !utf8Valid(out) {
		t.Fatalf("scrub cut inside a rune: %q", out)
	}
}

// secrets-2-2: the redaction covers the whole key-shaped run.
func TestSecretPatternRedactsFullRun(t *testing.T) {
	out := scrub([]byte("auth failed for oc_sk_ABC.DEFGHIJKLMNOPQRSTUV"), 512)
	if strings.Contains(out, "DEFGHIJKLMNOPQRSTUV") {
		t.Fatalf("charset edge leaked the key tail: %q", out)
	}
}

// round3-2: the buffered fallback flags the cap instead of silently truncating.
func TestStreamFallbackTruncationFlagged(t *testing.T) {
	old := responseCap
	responseCap = 64
	t.Cleanup(func() { responseCap = old })

	srv := chatServer(t, 200, strings.Repeat("y", 512), "")
	setBase(t, srv.URL)

	_, err := handleExecuteStream(execReq(`{"api_key":"k"}`, goodPayload))
	var structured *envelopeError
	if !errors.As(err, &structured) || structured.Code != "stream_truncated" {
		t.Fatalf("fallback must flag the cap, got %v", err)
	}
}

// round3-3: an event that never ends is flushed at the line bound, not at EOF.
func TestRelayEventBoundFlush(t *testing.T) {
	old := lineBufSize
	lineBufSize = 128
	t.Cleanup(func() { lineBufSize = old })

	var frames [][]byte
	err := relayFrames("s", strings.NewReader(strings.Repeat("data: x\n", 2048)), func(p []byte) error {
		frames = append(frames, append([]byte(nil), p...))
		return nil
	})
	if err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if len(frames) < 8 {
		t.Fatalf("an endless event must flush repeatedly, got %d frame(s)", len(frames))
	}
	for _, f := range frames {
		if len(f) > 256 {
			t.Fatalf("frame exceeded the event bound: %d bytes", len(f))
		}
	}
}

// round3-4: padding past the redaction window no longer rescues the tail.
func TestScrubRedactionHasNoPadBypass(t *testing.T) {
	hostile := "prefix oc_sk_" + strings.Repeat("A", 200) + "REALKEYTAIL suffix"
	out := scrub([]byte(hostile), 4096)
	if strings.Contains(out, "REALKEYTAIL") {
		t.Fatalf("padded key tail survived redaction: %q", out)
	}
}

// secrets-1-2 + inputs-1-1 + sched-1-4: structured, scrubbed, capped errors.
func TestUpstreamErrorScrubbedAndStructured(t *testing.T) {
	hostile := "oc_sk_SUPERSECRETVALUE0123456789 " + strings.Repeat("A", 4096)
	srv := chatServer(t, 401, hostile, "")
	setBase(t, srv.URL)

	_, err := handleExecute(execReq(`{"api_key":"k"}`, goodPayload))
	var structured *envelopeError
	if !errors.As(err, &structured) {
		t.Fatalf("expected a structured error, got %T %v", err, err)
	}
	if structured.HTTPStatus != 401 || !structured.CredentialScoped {
		t.Fatalf("401 must surface with credential scope: %+v", structured)
	}
	if strings.Contains(structured.Message, "SUPERSECRETVALUE") {
		t.Fatalf("key material must be redacted: %q", structured.Message)
	}
	if !strings.Contains(structured.Message, "oc_sk_[redacted]") {
		t.Fatalf("expected redaction marker, got %q", structured.Message)
	}
	if len(structured.Message) > 600 {
		t.Fatalf("error context must be capped, got %d bytes", len(structured.Message))
	}
}

// inputs-1-10 + sched-1-4: no silent fallback to a shared key.
func TestExecuteRequiresAuthCredential(t *testing.T) {
	for _, storage := range []string{``, `not json`, `{"label":"no key"}`} {
		_, err := handleExecute(execReq(storage, goodPayload))
		if err == nil || !strings.Contains(err.Error(), "no credential") {
			t.Fatalf("storage %q must fail loudly, got %v", storage, err)
		}
	}
}

// sched-1-4: the retry hint survives the error envelope.
func TestStreamUpstreamErrorCarriesRetryAfter(t *testing.T) {
	srv := chatServer(t, 429, "quota exceeded", "11")
	setBase(t, srv.URL)

	_, err := handleExecuteStream(execReq(`{"api_key":"k"}`, goodPayload))
	var structured *envelopeError
	if !errors.As(err, &structured) {
		t.Fatalf("expected structured error, got %v", err)
	}
	if structured.RetryAfterSeconds != 11 || structured.Code != "credential_quota" {
		t.Fatalf("429 must carry its hint and scope: %+v", structured)
	}
}

// inputs-1-7: CRLF framing is preserved in the fallback path.
func TestStreamFallbackFramesPreserveBytes(t *testing.T) {
	srv := chatServer(t, 200, "data: 1\r\n\r\ndata: 2\r\n\r\ndata: 3\r\n\r\n", "")
	setBase(t, srv.URL)

	out, err := handleExecuteStream(execReq(`{"api_key":"k"}`, goodPayload))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	var resp streamResponse
	if errDecode := json.Unmarshal(out, &resp); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if len(resp.Chunks) != 3 {
		t.Fatalf("CRLF frames must split, got %d chunk(s)", len(resp.Chunks))
	}
	for i, c := range resp.Chunks {
		if !strings.HasSuffix(string(c.Payload), "\r\n\r\n") {
			t.Fatalf("frame %d lost its CRLF delimiter: %q", i, c.Payload)
		}
	}
}

// A dial/TLS failure is a transport problem: the host must see
// transient_transport, not an untyped error that cools the model for a minute.
func TestTransportErrorIsTransient(t *testing.T) {
	setBase(t, "http://127.0.0.1:1") // nothing listens on port 1
	_, err := handleExecute(execReq(`{"api_key":"k"}`, goodPayload))
	var env *envelopeError
	if !errors.As(err, &env) || env.Code != "transient_transport" {
		t.Fatalf("want transient_transport, got %#v", err)
	}
}

// A client-caused 4xx must not cool the shared credential for every other client.
func TestClientErrorIsRequestScoped(t *testing.T) {
	cases := []int{400, 413, 422}
	for _, status := range cases {
		srv := chatServer(t, status, `{"error":"client sent nonsense"}`, "")
		setBase(t, srv.URL)
		_, err := handleExecute(execReq(`{"api_key":"k"}`, goodPayload))
		var env *envelopeError
		if !errors.As(err, &env) || env.Code != "request_scoped" || env.HTTPStatus != status {
			t.Fatalf("status %d: want request_scoped, got %#v", status, err)
		}
	}
}

// A client that brings its own conversation id must have it forwarded; the
// derived hash is only the fallback.
func TestUpstreamSessionPrefersClientHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-opencode-session")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	setBase(t, srv.URL)

	raw, _ := json.Marshal(map[string]any{
		"AuthID": "a", "Model": "hy4-flash-free", "Payload": []byte(goodPayload),
		"StorageJSON": []byte(`{"api_key":"k"}`),
		"Headers":     map[string][]string{"X-Session-ID": {"client-sess-42"}},
	})
	if _, err := handleExecute(raw); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if got != "client-sess-42" {
		t.Fatalf("upstream session = %q, want the client's id", got)
	}
}

// A hostile session header is sanitised instead of injected.
func TestUpstreamSessionSanitisesHeader(t *testing.T) {
	if got := upstreamSession(map[string][]string{"X-Session-ID": {"bad\r\nX-Evil: 1"}}, "m", nil); strings.ContainsAny(got, "\r\n\x00 ") {
		t.Fatalf("sanitised session still carries control or space bytes: %q", got)
	}
	if got := upstreamSession(nil, "m", []byte(goodPayload)); len(got) != 32 {
		t.Fatalf("fallback must be the 32-hex derived id, got %q", got)
	}
}

// The request cap is enforced before anything is copied further.
func TestPayloadCapRejected(t *testing.T) {
	old := requestCap
	requestCap = 64
	t.Cleanup(func() { requestCap = old })

	_, err := handleExecute(execReq(`{"api_key":"k"}`, strings.Repeat("x", 128)))
	var env *envelopeError
	if !errors.As(err, &env) || env.Code != "request_scoped" || env.HTTPStatus != 413 {
		t.Fatalf("want 413 request_scoped, got %#v", err)
	}
}

// Hostile upstream bodies reach the log; they must arrive as one line.
func TestScrubCollapsesControlBytes(t *testing.T) {
	got := scrub([]byte("line1\n[2026-11-19 03:00:00] forged\tline2\r\x00"), 200)
	if strings.ContainsAny(got, "\n\r\x00\t") {
		t.Fatalf("scrub left control bytes: %q", got)
	}
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Fatalf("scrub dropped content: %q", got)
	}
}

#!/usr/bin/env python3
"""Reference client for the OpenCode Go inference API.

Proves the point of this repo: `gemini-4-argon` answers a plain HTTPS request
with no opencode CLI, server or OAuth in the path. Reads keys from .env.

    python3 scripts/hello.py
    python3 scripts/hello.py --model hy4 --prompt "say hi" --stream
"""

import argparse
import json
import sys
import urllib.error
import urllib.request
import uuid
from pathlib import Path

DEFAULT_BASE = "https://opencode.ai/inference/go/openai/v1"
USER_AGENT = "opencode-go-reverse/0.1"
REPO_ROOT = Path(__file__).resolve().parent.parent


def load_env(path: Path) -> dict:
    """Parse KEY=VALUE lines from a .env file; missing file yields {}."""
    env = {}
    if not path.exists():
        return env
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        env[key.strip()] = value.strip().strip('"').strip("'")
    return env


def auth_headers(key, session=None) -> dict:
    """The three headers every call needs: bearer key, JSON, client name."""
    headers = {
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
        "User-Agent": USER_AGENT,
    }
    if session:
        headers["x-opencode-session"] = session
    return headers


def request_json(url, key, payload=None, session=None, timeout=180):
    #FUNC      One JSON round-trip against the inference API; returns (status, body).
    #NOT       Does not retry and does not raise on HTTP errors.
    #WHY       `x-opencode-session` is attached on every call because the Go gateway
    #          rejects requests without it (`MissingSessionID`), and the key goes into
    #          `Authorization` only — never into a URL, which would land in logs.
    headers = auth_headers(key, session)
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(
        url, data=data, headers=headers, method="POST" if data else "GET"
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, json.loads(response.read().decode())
    except urllib.error.HTTPError as error:
        body = error.read().decode(errors="replace")
        try:
            return error.code, json.loads(body)
        except json.JSONDecodeError:
            return error.code, {"raw": body}


def explain(status: int, body: dict) -> str:
    """Turn the three documented gates into one readable line."""
    #FUNC      Map an upstream failure to a human explanation of which gate refused.
    #WHY       `permission_error` and `isn't included` are different gates (org toggle vs
    #          plan); collapsing them would tell the user to fix the wrong thing.
    message = body.get("error", {}).get("message") or body.get("message") or str(body)[:200]
    if status == 403 and "Model access is disabled" in message:
        return f"403 org model-access toggle is off — {message}"
    if status == 400 and "isn't included in OpenCode Go" in message:
        return f"400 not part of the Go plan — {message}"
    if status == 402:
        return f"402 needs credits on the pay-as-you-go path — {message}"
    if "MissingSessionID" in message:
        return f"400 the request carried no x-opencode-session — {message}"
    return f"{status} {message}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", default="gemini-4-argon")
    parser.add_argument("--prompt", default="Reply with exactly: hello world")
    parser.add_argument("--base", default=None, help="override the Go base URL")
    parser.add_argument("--env", default=str(REPO_ROOT / ".env"))
    parser.add_argument("--stream", action="store_true")
    parser.add_argument("--list", action="store_true", help="only list models")
    args = parser.parse_args()

    env = load_env(Path(args.env))
    key = env.get("OC_KEY", "")
    base = (args.base or env.get("OC_GO_BASE") or DEFAULT_BASE).rstrip("/")
    if not key:
        print(f"no OC_KEY in {args.env}", file=sys.stderr)
        return 2

    session = f"hello-{uuid.uuid4()}"
    try:
        status, body = request_json(f"{base}/models", key)
        if status != 200:
            print(explain(status, body), file=sys.stderr)
            return 1
        models = [m["id"] for m in body.get("data", [])]
        print(f"models ({len(models)}): {', '.join(models)}")
        if args.model not in models:
            print(f"note: {args.model} is not in the advertised list — trying anyway")
        if args.list:
            return 0

        payload = {
            "model": args.model,
            "messages": [{"role": "user", "content": args.prompt}],
            "max_tokens": 64,
            "stream": args.stream,
        }
        if args.stream:
            print(f"\n== {args.model} (streaming) ==")
            return stream_chat(f"{base}/chat/completions", key, payload, session)
        status, body = request_json(f"{base}/chat/completions", key, payload, session)
        if status != 200:
            print(explain(status, body), file=sys.stderr)
            return 1
        choice = body["choices"][0]["message"]
        usage = body.get("usage", {})
        print(f"\n== {args.model} ==")
        print(choice.get("content"))
        print(
            f"\n[{body.get('model')}] tokens={usage.get('total_tokens')} "
            f"billing={usage.get('opencode', {}).get('billing_source')} "
            f"cost={usage.get('cost')}"
        )
        return 0
    except urllib.error.URLError as error:
        print(f"network error: {error}", file=sys.stderr)
        return 2


def stream_chat(url, key, payload, session) -> int:
    """Print SSE deltas as they arrive; returns a process exit code."""
    request = urllib.request.Request(
        url,
        data=json.dumps(payload).encode(),
        headers=auth_headers(key, session),
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=180) as response:
            for raw in response:
                line = raw.decode(errors="replace").strip()
                if not line.startswith("data: ") or line[6:] == "[DONE]":
                    continue
                chunk = json.loads(line[6:])
                for choice in chunk.get("choices", []):
                    piece = choice.get("delta", {}).get("content")
                    if piece:
                        print(piece, end="", flush=True)
        print()
        return 0
    except urllib.error.HTTPError as error:
        body = error.read().decode(errors="replace")
        try:
            print(explain(error.code, json.loads(body)), file=sys.stderr)
        except json.JSONDecodeError:
            print(f"{error.code} {body[:200]}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

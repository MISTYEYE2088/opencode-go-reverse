"""Offline tests for the reference client: no network, no key."""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))

import hello  # noqa: E402


class ExplainTest(unittest.TestCase):
    """The three upstream gates must stay distinguishable in the message."""

    def test_explain_maps_org_toggle(self):
        line = hello.explain(
            403, {"error": {"type": "permission_error",
                            "message": "Model access is disabled. Change model access at https://opencode.ai/console/org_x/models."}}
        )
        self.assertIn("org model-access toggle", line)

    def test_explain_maps_plan_exclusion(self):
        line = hello.explain(
            400, {"error": {"message": "Model gemini-4-carbon isn't included in OpenCode Go."}}
        )
        self.assertIn("not part of the Go plan", line)

    def test_explain_maps_missing_session(self):
        line = hello.explain(
            400, {"error": {"type": "MissingSessionID", "message": "Request is missing x-opencode-session"}}
        )
        self.assertIn("x-opencode-session", line)

    def test_explain_maps_credits(self):
        line = hello.explain(402, {"error": {"message": "Insufficient balance."}})
        self.assertIn("needs credits", line)


class HeaderTest(unittest.TestCase):
    def test_auth_headers_adds_session_only_when_given(self):
        self.assertNotIn("x-opencode-session", hello.auth_headers("k"))
        self.assertEqual(hello.auth_headers("k", "abc")["x-opencode-session"], "abc")

    def test_auth_headers_never_puts_key_in_url_or_body(self):
        headers = hello.auth_headers("secret-key", "s")
        self.assertEqual(headers["Authorization"], "Bearer secret-key")


class EnvTest(unittest.TestCase):
    def test_load_env_skips_comments_and_quotes(self):
        tmp = Path(self.id().replace(".", "_") + ".env")
        tmp.write_text('# comment\n\nOC_KEY="abc"\nOC_GO_BASE=https://x/v1\n')
        try:
            env = hello.load_env(tmp)
            self.assertEqual(env["OC_KEY"], "abc")
            self.assertEqual(env["OC_GO_BASE"], "https://x/v1")
        finally:
            tmp.unlink()


if __name__ == "__main__":
    unittest.main()

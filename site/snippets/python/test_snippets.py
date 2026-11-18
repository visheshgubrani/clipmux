"""The Python snippets against the shared vector files.

Run: python -m unittest test_snippets (from this directory, with
requirements.txt installed), or `make snippets-test`.
"""

import base64
import json
import pathlib
import unittest

import jwt

from playback_token import playback_token
from verify_webhook import WebhookError, verify_webhook

ROOT = pathlib.Path(__file__).resolve().parents[3]
WEBHOOKS = json.loads((ROOT / "internal/webhooks/testdata/vectors.json").read_text())
DELIVERY = json.loads((ROOT / "internal/delivery/testdata/vectors.json").read_text())


def b64json(part: str) -> dict:
    return json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)))


class Webhooks(unittest.TestCase):
    def test_vectors(self):
        for case in WEBHOOKS["cases"]:
            with self.subTest(case["name"]):
                body = case["body"].encode()
                try:
                    verify_webhook(case["secret"], body, case["headers"], now=case["now"])
                    got = (True, None)
                except WebhookError as e:
                    got = (False, e.reason)
                self.assertEqual(got, (case["valid"], case.get("reason")))


class Tokens(unittest.TestCase):
    def test_signs_what_delivery_reads(self):
        want = DELIVERY["token_claims"]["v"]
        key = next(k for k in DELIVERY["signing_keys"] if k["kid"] == want["kid"])
        token = playback_token(want["sub"], key["private_key_pem"], want["kid"], viewer_id=want["vid"])

        head, payload, sig = token.split(".")
        self.assertNotIn("=", token)
        self.assertEqual(len(base64.urlsafe_b64decode(sig + "=" * (-len(sig) % 4))), 64)
        self.assertEqual(b64json(head), {"alg": "ES256", "kid": want["kid"], "typ": "JWT"})
        claims = b64json(payload)
        for name in ("sub", "aud", "vid"):
            self.assertEqual(claims[name], want[name])
        self.assertEqual(claims["exp"] - claims["iat"], 900)
        self.assertLessEqual(claims["exp"] - claims["iat"], DELIVERY["max_token_lifetime_seconds"])
        jwt.decode(token, key["public_key_pem"], algorithms=["ES256"], audience="v")


if __name__ == "__main__":
    unittest.main()

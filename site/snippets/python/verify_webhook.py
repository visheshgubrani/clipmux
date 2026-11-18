import hashlib
import hmac
import json
import re
import time


class WebhookError(Exception):
    """Answer 400. reason is "missing_header", "stale" or "bad_signature"."""

    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


def verify_webhook(secret: str, raw_body: bytes, headers, tolerance: int = 300, now: int | None = None) -> dict:
    """Check a ClipMux webhook and return its event.

    secret is CLIPMUX_WEBHOOK_SECRET, whsec_ prefix included. raw_body is the
    request body exactly as it arrived: parsed and serialized again, it no
    longer matches. headers is your framework's (case-insensitive) headers.
    """
    timestamp = headers.get("X-Webhook-Timestamp")
    signature = headers.get("X-Webhook-Signature")
    if not timestamp or not signature:
        raise WebhookError("missing_header")
    now = int(time.time()) if now is None else now
    if not re.fullmatch(r"[0-9]+", timestamp) or abs(now - int(timestamp)) > tolerance:
        raise WebhookError("stale")
    mac = hmac.new(secret.encode(), timestamp.encode() + b"." + raw_body, hashlib.sha256)
    if not hmac.compare_digest(signature.encode(), f"sha256={mac.hexdigest()}".encode()):
        raise WebhookError("bad_signature")
    return json.loads(raw_body)

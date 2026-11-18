import time

import jwt  # pip install "pyjwt[crypto]"


def playback_token(
    playback_id: str,
    signing_key_pem: str,
    kid: str,
    viewer_id: str | None = None,
    audience: str = "v",
    ttl: int = 900,
) -> str:
    """A token for a signed playback ID, signed with your app's signing key.

    signing_key_pem is CLIPMUX_SIGNING_KEY and kid is CLIPMUX_SIGNING_KID.
    audience is "v" to play, or "t" for the poster. viewer_id, when given,
    counts unique viewers; ClipMux keeps only a keyed hash of it.
    """
    now = int(time.time())
    claims = {"sub": playback_id, "aud": audience, "iat": now, "exp": now + ttl}
    if viewer_id:
        claims["vid"] = viewer_id
    return jwt.encode(claims, signing_key_pem, algorithm="ES256", headers={"kid": kid})

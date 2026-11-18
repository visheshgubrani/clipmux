<?php

use Firebase\JWT\JWT; // composer require firebase/php-jwt

/**
 * A token for a signed playback ID, signed with your app's signing key.
 *
 * $signingKeyPem is CLIPMUX_SIGNING_KEY and $kid is CLIPMUX_SIGNING_KID.
 * $audience is "v" to play, or "t" for the poster. $viewerId, when given,
 * counts unique viewers; ClipMux keeps only a keyed hash of it.
 */
function clipmux_playback_token(
    string $playbackId,
    string $signingKeyPem,
    string $kid,
    ?string $viewerId = null,
    string $audience = 'v',
    int $ttl = 900,
): string {
    $now = time();
    $claims = ['sub' => $playbackId, 'aud' => $audience, 'iat' => $now, 'exp' => $now + $ttl];
    if ($viewerId !== null && $viewerId !== '') {
        $claims['vid'] = $viewerId;
    }
    return JWT::encode($claims, $signingKeyPem, 'ES256', $kid);
}

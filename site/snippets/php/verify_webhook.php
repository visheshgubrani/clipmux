<?php

/** Answer 400. getMessage() is "missing_header", "stale" or "bad_signature". */
class ClipmuxWebhookError extends Exception
{
}

/**
 * Checks a ClipMux webhook and returns its event.
 *
 * $secret is CLIPMUX_WEBHOOK_SECRET, whsec_ prefix included, and $rawBody is
 * the request body exactly as it arrived (file_get_contents('php://input')).
 * $timestamp and $signature are the X-Webhook-Timestamp and
 * X-Webhook-Signature headers.
 */
function clipmux_verify_webhook(
    string $secret,
    string $rawBody,
    ?string $timestamp,
    ?string $signature,
    int $tolerance = 300,
    ?int $now = null,
): array {
    if ($timestamp === null || $timestamp === '' || $signature === null || $signature === '') {
        throw new ClipmuxWebhookError('missing_header');
    }
    $now ??= time();
    if (!preg_match('/\A[0-9]+\z/', $timestamp) || abs($now - (int) $timestamp) > $tolerance) {
        throw new ClipmuxWebhookError('stale');
    }
    $expected = 'sha256=' . hash_hmac('sha256', $timestamp . '.' . $rawBody, $secret);
    if (!hash_equals($expected, $signature)) {
        throw new ClipmuxWebhookError('bad_signature');
    }
    return json_decode($rawBody, true, 512, JSON_THROW_ON_ERROR);
}

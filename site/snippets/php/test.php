<?php
// The PHP snippets against the shared vector files. Run: php test.php
// (after composer install), or `make snippets-test`.

require __DIR__ . '/vendor/autoload.php';
require __DIR__ . '/playback_token.php';
require __DIR__ . '/verify_webhook.php';

use Firebase\JWT\JWT;
use Firebase\JWT\Key;

$root = dirname(__DIR__, 3);
$webhooks = json_decode(file_get_contents("$root/internal/webhooks/testdata/vectors.json"), true);
$delivery = json_decode(file_get_contents("$root/internal/delivery/testdata/vectors.json"), true);
$failures = 0;

function check(bool $ok, string $what): void
{
    global $failures;
    if (!$ok) {
        $failures++;
        fwrite(STDERR, "FAIL $what\n");
    }
}

function b64json(string $part): array
{
    return json_decode(base64_decode(strtr($part, '-_', '+/')), true);
}

foreach ($webhooks['cases'] as $case) {
    $h = $case['headers'];
    try {
        clipmux_verify_webhook($case['secret'], $case['body'], $h['X-Webhook-Timestamp'] ?? null, $h['X-Webhook-Signature'] ?? null, 300, $case['now']);
        $got = [true, null];
    } catch (ClipmuxWebhookError $e) {
        $got = [false, $e->getMessage()];
    }
    check($got === [$case['valid'], $case['reason'] ?? null], "webhook {$case['name']}: " . json_encode($got));
}

$want = $delivery['token_claims']['v'];
$key = array_values(array_filter($delivery['signing_keys'], fn ($k) => $k['kid'] === $want['kid']))[0];
$token = clipmux_playback_token($want['sub'], $key['private_key_pem'], $want['kid'], $want['vid']);
[$head, $payload, $sig] = explode('.', $token);
check(!str_contains($token, '='), 'token is unpadded base64url');
check(strlen(base64_decode(strtr($sig, '-_', '+/'))) === 64, 'signature is 64 bytes');
$header = b64json($head);
ksort($header);
check($header === ['alg' => 'ES256', 'kid' => $want['kid'], 'typ' => 'JWT'], 'header ' . json_encode($header));
$claims = (array) JWT::decode($token, new Key($key['public_key_pem'], 'ES256'));
check($claims['sub'] === $want['sub'] && $claims['aud'] === 'v' && $claims['vid'] === $want['vid'], 'claims ' . json_encode($claims));
check($claims['exp'] - $claims['iat'] === 900 && 900 <= $delivery['max_token_lifetime_seconds'], 'lifetime');

echo $failures === 0 ? 'ok: ' . count($webhooks['cases']) . " webhook cases, token\n" : "$failures failed\n";
exit($failures === 0 ? 0 : 1);

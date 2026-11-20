#!/usr/bin/env node
// Stands in for the server's owner during a skill eval: approves the app
// pairing that `npx @clipmux/node init` printed, as the /connect page does,
// through the management API. It creates a new app named "Course site",
// marked as running on this computer, with the webhook URL given.
//
//   node skills/evals/approve.mjs <Approve URL> [webhook URL]
//
// The owner's credentials come from the exit-8 fixture's /config
// (CLIPMUX_EVAL_FIXTURE, default http://127.0.0.1:8097).

const [approveUrl, webhookUrl = 'http://127.0.0.1:3000/api/clipmux/webhook'] = process.argv.slice(2)
if (!approveUrl) {
  console.error('usage: node approve.mjs <Approve URL> [webhook URL]')
  process.exit(2)
}

const fixture = process.env.CLIPMUX_EVAL_FIXTURE ?? 'http://127.0.0.1:8097'
const cfg = await (await fetch(`${fixture}/config`)).json()
const url = new URL(approveUrl)
const code = url.searchParams.get('code')
const base = url.origin
if (base !== new URL(cfg.ops).origin) throw new Error(`${base} is not the fixture's server ${cfg.ops}`)
if (!code) throw new Error('the URL has no ?code=')

const signIn = await fetch(`${base}/v1/session`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', Origin: base },
  body: JSON.stringify({ email: cfg.ops_email, password: cfg.ops_password }),
})
if (!signIn.ok) throw new Error(`sign-in: ${signIn.status} ${await signIn.text()}`)
const cookie = (signIn.headers.getSetCookie?.() ?? []).map((c) => c.split(';')[0]).join('; ')

const res = await fetch(`${base}/v1/app-pairings/${encodeURIComponent(code)}/approve`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', Origin: base, Cookie: cookie },
  body: JSON.stringify({ new_app_name: 'Course site', webhook_url: webhookUrl, local: true }),
})
const body = await res.json()
if (!res.ok) throw new Error(`approve: ${res.status} ${JSON.stringify(body)}`)
console.log(`approved pairing ${code}: ${body.status}, webhook ${webhookUrl}`)

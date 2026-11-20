# Skill evals

Launch exit check 7 (P6-T17, D112): a coding agent with only the installed
skills and a running ClipMux adds upload, the webhook and signed playback to
a sample app, the video plays, and the API key never reaches client code.

| Path                                                          | What it is                                                                                                |
| ------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| [`lms-starter/`](lms-starter)                                 | The sample app: a Next.js course site with lessons, three stand-in users and access rules, and no ClipMux |
| [`lms-upload-webhook-playback/`](lms-upload-webhook-playback) | The eval case, in skill-creator's layout (`prompt.md`, `case.yaml`, one grader per file)                  |
| [`runs/`](runs)                                               | One record per eval run: the graders, their evidence, and what the run found in the skills              |
| [`approve.mjs`](approve.mjs)                                  | Stands in for the server's owner: approves the pairing `init` prints, through the management API          |
| [`../../e2e/web/exit7`](../../e2e/web/exit7)                  | The check that grades a finished app by using it in Chrome                                                |

The case lives here, not in each skill's `evals/`, because it exercises both
skills at once, and `npx skills add` copies a skill's whole folder into the
user's project.

## Run it

Each run is a billed agent session and takes 20–40 minutes. It needs
`TEST_DATABASE_URL`, ffmpeg, ffprobe, Shaka Packager and Chrome, as exit 8
does.

1. **Start the instance**, the exit-8 fixture: serve and a worker with an
   owner account and a 45 s clip. Leave it running.

   ```bash
   cd e2e/web && CLIPMUX_E2E=1 CLIPMUX_E2E_EXAMPLE_FIXTURE=1 CLIPMUX_FIXTURE_PORT=8097 go test ../../internal/e2e -run TestExampleFixture -count=1 -timeout 90m -v
   ```

   It prints `ClipMux http://127.0.0.1:<port>`: that is `{{CLIPMUX_URL}}`.
2. **Make the agent's project:** copy `lms-starter` somewhere outside the
   repository, `git init` it (so the skills install into the project), and
   install the skills from the branch under test:

   ```bash
   npx skills add <path to this checkout, or visheshgubrani/clipmux> --skill '*' -a claude-code -y --copy
   ```

   A branch on GitHub is `visheshgubrani/clipmux#<branch>`; the
   `…/tree/<branch>` URL form fails when the branch name has a slash.

3. **Run the agent** in that directory with `prompt.md`'s prompt, replacing
   `{{CLIPMUX_URL}}` and `{{APPROVE}}` (`node <checkout>/skills/evals/approve.mjs`).
   Tell it that its ClipMux knowledge comes only from the installed skills,
   the clipmux.com pages they link to and the packages in `node_modules`, not
   from this repository. Keep its transcript.
4. **Grade it** with the exit-7 check, against the same instance:

   ```bash
   cd e2e/web && CLIPMUX_EVAL_APP=<the agent's project> CLIPMUX_E2E_CHANNEL=chrome pnpm run test:exit7
   ```

   Then grade the case's other statements (`graders/`) from the transcript and
   the diff, and record the run in the pull request that changes the skills.

`approve.mjs` names the new app "Course site", so a second run on the same
instance needs a fresh fixture (stop it and start it again).

## When to run it

Before a release, and in any pull request that changes `skills/`, the
packages' public API, or the integration API's error codes. `make
site-check` covers the parts that need no agent: every docs link and anchor
the skills use resolves and is pinned (`## Heading [#id]`), every error code
in the spec is in `clipmux-integrate/references/errors.md`, and each
`SKILL.md`'s frontmatter parses.

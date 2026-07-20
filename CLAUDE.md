# CLAUDE.md — working on plico

plico is a pull-based GitOps deployer for standalone Docker Compose stacks.
Read `README.md` for what it does and `ROADMAP.md` for what is in/out of
scope and why. This file is about *how to work in this repo*.

## The one rule: orchestrate CLIs, never reimplement

plico shells out to `git`, `sops` and `docker compose` via `os/exec`. It does
**not** embed go-git, compose-go, the sops library, or the Docker SDK. This is
the whole design thesis (it is what makes plico small and doco-cd large). When
tempted to add a Go library that duplicates one of those CLIs, don't — extend
the orchestration instead. The target is a codebase two people can hold in
their heads.

## Tooling

- **mise** manages tool versions AND tasks (no Makefile/Taskfile). Everything
  runs through it: `mise run build | test | lint | fmt | smoke | shellcheck |
  install-test | release-test`. Tool versions are pinned in `mise.toml`; never
  bump Go/golangci-lint/sops/age silently.
- Git hooks run **outside** the mise environment, so `lefthook.yml` invokes
  every tool via `mise exec --`. Keep that wrapper if you touch it.
- Go 1.26.5. Two direct deps only: `BurntSushi/toml`, `spf13/cobra` (no viper),
  plus `robfig/cron/v3`. Adding a dependency needs a real justification.

## Local verification is the pre-push gate

Pushes are **approval-gated** (see Workflow), so you cannot lean on CI to catch
things before the human sees them. Local verification is the real gate. Before
presenting work as ready:

1. `mise run fmt` (gofmt + go vet)
2. `mise run lint` — must be **0 issues**
3. `mise run test` — all green
4. `mise run smoke` for anything touching the deploy pipeline, scheduler,
   sops, notifications, or the CLI. It stands up a real git repo + Docker
   stack + sops/age + a local ntfy capture and asserts end to end. It is the
   test that catches what unit tests with fakes cannot (it caught the real
   `sops exec-env` argv contract, and a Linux `mktemp` portability bug).

CI (lint + unit + shellcheck + installer matrix + smoke) runs once the branch
is pushed after approval — it confirms the ubuntu/macos matrix, it is not a
substitute for running the above locally first.

## Package map (`internal/`)

`execx` (command runner + FakeRunner — the testability seam) · `config` (TOML,
`${ENV}` interpolation, validation) · `state` (atomic state.json) · `notify`
(Notifier + ntfy/webhook/smtp + event filter) · `gitrepo` · `compose`
(`Runtime` interface — the Podman extension point) · `sopsx` · `hooks` ·
`deploy` (the per-stack pipeline) · `scheduler` (poll loop + cron windows) ·
`api` (/healthz + unix-socket client API). The CLI lives in `cmd/plico`.

## Non-negotiable behaviours — do not "simplify" these away

- **Fail loudly, never silently.** A declared-but-unusable thing (a pre-hook
  missing its +x bit, a configured hook that's gone, an invalid config) must
  block/refuse, not skip. This is why F23 (ignore-invalid-stack) was rejected.
- **The pre-deploy hook is a blocking gate.** `.deploy/pre-deploy.sh exit != 0`
  ⇒ no deploy, ever, except a deliberate `deploy-now --skip-pre --force` (loud
  + notified). The hook is a *general* pre/post-deploy mechanism — backup is
  the headline use case, but it can equally quiesce a service, run a migration,
  check preconditions, or anything else. Generic pre/post hooks are precisely
  what doco-cd (distroless, no shell) cannot do; do not narrow the hook back
  down to "backup".
- **Secrets never hit disk in cleartext.** sops decrypts into the process env
  (`sops exec-env`) or a tmpfs, never a file under `base_dir`. The smoke test
  asserts no secret leaks into base_dir, logs, or notifications — keep it that
  way.
- **A failing notifier must never break or stall a deployment** (`WithLogFallback`,
  bounded timeouts, async emission from the scheduler tick).
- **Config changes apply via `systemctl restart`**, which drains in-flight
  runs and restores persisted schedule anchors. There is no SIGHUP (rejected).

## The scheduler is the sharp edge

`internal/scheduler` (cron windows + persisted anchors) is by far the most
subtle code here. Its window state machine took **three consecutive rounds of
adversarial review** (13 → 10 → 2 findings) to get right, plus a fourth after
notifications were wired in. The invariants: the window is authoritative
(never deploy outside it), a missed window is logged+notified but never
replayed late, the anchor (last firing + the cron expr it was computed under)
is persisted so restarts resume correctly, and run attribution is captured at
dispatch. **Any change here — however small — warrants an adversarial code
review** (`/code-review high`). Do not trust "it's obviously fine".

## Testing: test-first (TDD)

Write the test before the implementation. This fits plico: almost every bug a
review has caught was a *logic* bug (scheduler state machine, notification
dedup, thresholds) that a test written first would have pinned down. Concretely:

1. Write a failing test that states the behaviour (or the bug being fixed).
2. Implement until it passes.
3. Refactor with the test as the safety net.

For work whose shape is still uncertain (a new interface, an integration whose
contract you don't yet know), the first "test" may start as a sketch you refine
as the API emerges — that is iterative test-first, not an excuse to skip it.

Conventions:
- Stdlib `testing` only, table-driven, `t.Parallel()`. No testify.
- Inject `execx.FakeRunner` everywhere a CLI is called — unit tests spawn no
  real git/docker/sops.
- The scheduler is time-driven: use `NewAt(..., now)` with fixed timestamps,
  never wall-clock, so tests are deterministic.
- A review-found bug and its regression test land together.

## Git workflow

- **One branch per unit of work** (feature, fix, doc). Branch off `main`; never
  commit straight to `main`.
- **Never push without explicit approval.** Do the local verification above,
  present the result, and push (+ open the PR) only once the human says so.
- **Amend, don't pile up fix commits — while the work is in flight.** Review
  findings and follow-up corrections fold back into the branch's commit(s) via
  `git commit --amend` (or an interactive rebase for a multi-commit branch),
  and the commit message is updated to describe the final, hardened state. One
  round-trip = one clean commit, not feature+fix+fix.
- **The boundary:** amend applies only to unmerged, in-flight work on its
  branch. A fix to code already merged into `main` is a *new* commit on a *new*
  branch — never rewrite shared history.

## Working with code reviews

Substantial changes (pipeline, scheduler, socket, notifications) get a
`/code-review high` **on the branch before merge**: review → amend the fixes in
→ re-review until it converges → merge on approval. This has been the actual
pattern and it pays — most rounds surface real, non-obvious concurrency and
lifecycle bugs. The review trail does not need to live in git history (it would
be folded away by the amends anyway); decisions live in commit messages and the
ROADMAP. If a review's verifier agents fail (e.g. session limits), its "no
findings" verdict is not trustworthy — re-run it.

## Doc & language

Everything in this repo is written in **English** — code, comments, README,
docs/, this file. The **only** exception is `ROADMAP.md`, kept in French for
now. Commit messages explain the *why*, especially for rejected alternatives
and non-obvious decisions — future-you re-reads them instead of re-deciding.

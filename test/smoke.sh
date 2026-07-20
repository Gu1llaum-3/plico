#!/bin/sh
# End-to-end smoke test for plico: real git repo, real docker compose, real
# sops+age encryption. Run it with `mise run smoke` (needs git, docker, and
# the mise-pinned sops/age in PATH).
#
# Covers:
#   - clone/fetch + deploy on git delta, hook context (F11), healthz, state
#   - SOPS gate (F16): PARTIALLY encrypted dotenv (.sops.yaml with
#     encrypted_regex — the recommended diff-friendly workflow) decrypted in
#     memory via `sops exec-env`; both clear and secret values reach the
#     container env, the secret cleartext never touches base_dir nor logs
#   - backup gate (F12/F14): failing pre-deploy hook blocks the new revision
set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLICO="$REPO_ROOT/bin/plico"
# Portable across GNU and BSD mktemp (GNU requires XXXXXX in the template).
WS="$(mktemp -d "${TMPDIR:-/tmp}/plico-smoke.XXXXXX")"
# Everything below builds paths from $WS: refuse to run with an empty one,
# otherwise the script would operate on / and on the plico checkout itself.
[ -n "$WS" ] && [ -d "$WS" ] || { echo "mktemp failed"; exit 1; }
ORIGIN="$WS/origin.git"
WORK="$WS/work"
BASE="$WS/base"
MARKER="$WS/marker"
LOG="$WS/plico.log"
PORT=19444
SECRET="s3cret-from-sops-42"
FAILURES=""

fail() { FAILURES="$FAILURES\n- $1"; echo "FAIL: $1"; }
ok() { echo "OK: $1"; }

cleanup() {
  [ -n "${DAEMON_PID:-}" ] && kill "$DAEMON_PID" 2>/dev/null
  [ -n "${MONO_PID:-}" ] && kill "$MONO_PID" 2>/dev/null
  [ -n "${NTFY_PID:-}" ] && kill "$NTFY_PID" 2>/dev/null
  [ -n "${HB_PID:-}" ] && kill "$HB_PID" 2>/dev/null
  docker compose -p smoke down --timeout 2 >/dev/null 2>&1
  docker compose -p mono-a down --timeout 2 >/dev/null 2>&1
  docker compose -p mono-b down --timeout 2 >/dev/null 2>&1
  rm -rf "$WS"
}
trap cleanup EXIT

command -v sops >/dev/null || { echo "sops not in PATH (run via: mise run smoke)"; exit 1; }
command -v age-keygen >/dev/null || { echo "age-keygen not in PATH (run via: mise run smoke)"; exit 1; }
[ -x "$PLICO" ] || { echo "bin/plico missing (run: mise run build)"; exit 1; }

mkdir -p "$BASE"

# ── 1. age key + origin repo with sops-encrypted secrets ───────────────
age-keygen -o "$WS/age.key" 2>/dev/null
RECIPIENT=$(age-keygen -y "$WS/age.key")

git init -q --bare -b main "$ORIGIN"
git init -q -b main "$WORK"
cd "$WORK" || exit 1
git config user.email smoke@test && git config user.name smoke

cat > docker-compose.yml <<'EOF'
services:
  app:
    image: alpine:3.20
    command: ["sleep", "300"]
    environment:
      SECRET_MESSAGE: ${SECRET_MESSAGE}
      PUBLIC_MESSAGE: ${PUBLIC_MESSAGE}
EOF

# Partial encryption, the diff-friendly workflow: .sops.yaml declares which
# keys must be encrypted (encrypted_regex); everything else stays readable
# in git diffs. mac_only_encrypted lets cleartext values be edited without
# going through `sops edit`.
cat > .sops.yaml <<EOF
creation_rules:
  - path_regex: \.deploy/.*\.enc\.env\$
    encrypted_regex: "(SECRET|PASSWORD|TOKEN|KEY)"
    mac_only_encrypted: true
    age: $RECIPIENT
EOF

mkdir -p .deploy
printf 'PUBLIC_MESSAGE=hello-in-clear\nSECRET_MESSAGE=%s\n' "$SECRET" > .deploy/secrets.enc.env
sops encrypt --in-place .deploy/secrets.enc.env
grep -q "SECRET_MESSAGE=ENC\[" .deploy/secrets.enc.env \
  && ok "secret key is encrypted in the committed file" \
  || fail "SECRET_MESSAGE not encrypted (encrypted_regex not applied?)"
grep -q "PUBLIC_MESSAGE=hello-in-clear" .deploy/secrets.enc.env \
  && ok "non-secret key stays readable for git diff" \
  || fail "PUBLIC_MESSAGE should stay in clear (partial encryption)"

cat > .deploy/pre-deploy.sh <<EOF
#!/bin/sh
echo "backup for \$DEPLOY_STACK: \$DEPLOY_OLD_SHA -> \$DEPLOY_NEW_SHA" >> "$MARKER"
# Env scoping: SOPS_AGE_KEY_FILE is a daemon secret and must NOT be visible
# to a repo-controlled hook; PLICO_SMOKE_PASS is opt-in via env_passthrough.
echo "age=[\${SOPS_AGE_KEY_FILE:-}] pass=[\${PLICO_SMOKE_PASS:-}]" >> "$MARKER"
exit 0
EOF
chmod +x .deploy/pre-deploy.sh
git add -A && git commit -qm "v1"
git remote add origin "$ORIGIN"
git push -q origin main
SHA1=$(git rev-parse HEAD)

# ── 2. local ntfy capture: notifications are validated END TO END ──────
NTFY_LOG=$WS/ntfy-capture.log
NTFY_PORT=${PLICO_SMOKE_NTFY_PORT:-19555}
: >"$NTFY_LOG"
python3 -c '
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n).decode(errors="replace")
        with open(sys.argv[1], "a") as f:
            f.write(self.headers.get("Title", "") + "|" + body.replace("\n", " ") + "\n")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[2])), H).serve_forever()
' "$NTFY_LOG" "$NTFY_PORT" &
NTFY_PID=$!

# ── 2b. local heartbeat capture: the liveness push is validated too ────
HB_LOG=$WS/heartbeat-capture.log
HB_PORT=${PLICO_SMOKE_HB_PORT:-19556}
: >"$HB_LOG"
python3 -c '
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(sys.argv[1], "a") as f:
            f.write("beat\n")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[2])), H).serve_forever()
' "$HB_LOG" "$HB_PORT" &
HB_PID=$!

# ── 3. plico config with sops and ntfy enabled on the stack ────────────
cat > "$WS/config.toml" <<EOF
base_dir = "$BASE"
poll_interval = "5s"
run_timeout = "5m"

[health]
listen = "127.0.0.1:$PORT"

[heartbeat]
url = "http://127.0.0.1:$HB_PORT/push"
interval = "5s"

[ntfy]
url = "http://127.0.0.1:$NTFY_PORT/plico"
events = ["all"]

[hooks]
# PLICO_SMOKE_PASS is opted in for the hook; SOPS_AGE_KEY_FILE is NOT.
env_passthrough = ["PLICO_SMOKE_PASS"]

[[stack]]
name = "smoke"
repo = "file://$ORIGIN"
sops_files = [".deploy/secrets.enc.env"]
verify_timeout = "60s"
EOF

# ── 4. daemon: the age key reaches sops through plico's environment ────
SOPS_AGE_KEY_FILE="$WS/age.key" PLICO_SMOKE_PASS="passed-through" "$PLICO" serve --config "$WS/config.toml" > "$LOG" 2>&1 &
DAEMON_PID=$!

i=0
while [ $i -lt 90 ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" 2>/dev/null || echo 000)
  if [ "$code" = "200" ] && grep -q '"last_status": *"success"' "$BASE/state.json" 2>/dev/null; then
    break
  fi
  i=$((i+1)); sleep 1
done
[ "$code" = "200" ] && ok "healthz returns 200" || fail "healthz never returned 200 (last: $code)"

grep -q "$SHA1" "$BASE/state.json" 2>/dev/null \
  && ok "state.json records SHA1" || fail "state.json missing SHA1"
grep -q "backup for smoke:  -> $SHA1" "$MARKER" 2>/dev/null \
  && ok "pre-deploy hook ran with deploy context" || fail "hook marker missing/wrong"
# Env scoping: the age key must be withheld, the passthrough var present.
grep -q "age=\[\] pass=\[passed-through\]" "$MARKER" 2>/dev/null \
  && ok "hook env scoped: age key withheld, passthrough var present" \
  || fail "hook env scoping wrong: $(grep '^age=' "$MARKER" 2>/dev/null)"
docker compose -p smoke ps --format '{{.Service}} {{.State}}' 2>/dev/null | grep -q "app running" \
  && ok "container app is running" || fail "container not running"

# ── 5. SOPS and notification assertions ────────────────────────────────
CID=$(docker compose -p smoke ps -q app)
CONTAINER_ENV=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$CID" 2>/dev/null)
echo "$CONTAINER_ENV" | grep -q "SECRET_MESSAGE=$SECRET" \
  && ok "decrypted secret injected into container env via sops exec-env" \
  || fail "SECRET_MESSAGE not present in container env"
echo "$CONTAINER_ENV" | grep -q "PUBLIC_MESSAGE=hello-in-clear" \
  && ok "clear value from the same file injected too" \
  || fail "PUBLIC_MESSAGE not present in container env"
if grep -R -q "$SECRET" "$BASE" 2>/dev/null; then
  fail "cleartext secret found under base_dir"
else
  ok "no cleartext secret under base_dir"
fi
if grep -q "$SECRET" "$LOG" 2>/dev/null; then
  fail "cleartext secret leaked into plico log"
else
  ok "no cleartext secret in plico log"
fi
grep -q "smoke: deploy_success" "$NTFY_LOG" 2>/dev/null \
  && ok "deploy_success notification received end to end" \
  || fail "deploy_success never reached the ntfy capture"
if grep -q "$SECRET" "$NTFY_LOG" 2>/dev/null; then
  fail "cleartext secret leaked into a notification"
else
  ok "no cleartext secret in notifications"
fi
# Liveness heartbeat: the daemon is healthy with a 5s beat. Wait up to two
# intervals for the first outbound push (the deploy can finish faster than
# one beat when the image is cached).
i=0
while [ $i -lt 12 ] && [ ! -s "$HB_LOG" ]; do i=$((i+1)); sleep 1; done
[ -s "$HB_LOG" ] \
  && ok "liveness heartbeat pushed while healthy" \
  || fail "no heartbeat reached the capture"

# ── 6. client CLI over the unix socket ─────────────────────────────────
"$PLICO" validate --config "$WS/config.toml" >/dev/null 2>&1 \
  && ok "validate accepts the config" || fail "validate failed"
"$PLICO" status --config "$WS/config.toml" 2>/dev/null | grep -q "smoke.*success" \
  && ok "status reports the stack over the socket" || fail "status did not report success"
"$PLICO" dry-run --stack smoke --config "$WS/config.toml" 2>/dev/null | grep -q "up to date" \
  && ok "dry-run reports up to date" || fail "dry-run wrong"
DN=$("$PLICO" deploy-now --stack smoke --force --config "$WS/config.toml" 2>&1)
echo "$DN" | grep -q "smoke: deployed" \
  && ok "deploy-now --force redeploys via the socket" || fail "deploy-now: $DN"
"$PLICO" deploy-now --stack smoke --skip-pre --config "$WS/config.toml" >/dev/null 2>&1 \
  && fail "--skip-pre without --force must be refused (F30)" \
  || ok "--skip-pre without --force refused (F30)"

# ── 7. gate test: failing pre-deploy hook must block the new revision ──
cd "$WORK" || exit 1
sed -i.bak 's/"300"/"301"/' docker-compose.yml && rm -f docker-compose.yml.bak
cat > .deploy/pre-deploy.sh <<'EOF'
#!/bin/sh
echo "pg_dump: connection refused" >&2
exit 1
EOF
chmod +x .deploy/pre-deploy.sh
git add -A && git commit -qm "v2 with broken backup" && git push -q origin main

i=0
while [ $i -lt 60 ]; do
  grep -q '"last_status": *"pre_hook_failed"' "$BASE/state.json" 2>/dev/null && break
  i=$((i+1)); sleep 1
done
grep -q '"last_status": *"pre_hook_failed"' "$BASE/state.json" \
  && ok "gate failure recorded in state" || fail "pre_hook_failed never recorded"
grep -q "\"last_deployed_sha\": *\"$SHA1\"" "$BASE/state.json" \
  && ok "SHA stays at v1 after gate failure (F12)" || fail "state SHA moved despite failed gate"
docker inspect --format '{{join .Args " "}}' "$CID" 2>/dev/null | grep -q "300" \
  && ok "running container still v1 (sleep 300)" || fail "container was redeployed despite failed gate"
grep -q "pre-deploy hook failed" "$LOG" \
  && ok "failure logged" || fail "no failure log entry"
grep -q "pg_dump: connection refused" "$LOG" \
  && ok "hook stderr captured in log (F14)" || fail "hook stderr not in log"
grep -q "smoke: pre_hook_failed" "$NTFY_LOG" 2>/dev/null \
  && grep "smoke: pre_hook_failed" "$NTFY_LOG" | grep -q "pg_dump: connection refused" \
  && ok "pre_hook_failed notification carries the hook stderr end to end" \
  || fail "pre_hook_failed notification missing or without hook stderr"

# ── 8. monorepo: two stacks in one repo, path-scoped change detection ──
# One repo, two subdirs (a/, b/), each a stack via `path`. Proves end to end
# (real git diff, real compose cwd) that a commit touching only a/ redeploys
# stack "a" alone and leaves "b" untouched — the guarantee that makes `path`
# usable in a monorepo.
MONO_ORIGIN="$WS/mono-origin.git"
MONO_WORK="$WS/mono-work"
MONO_BASE="$WS/mono-base"
MONO_MDIR="$WS/mono-markers"   # per-stack hook markers, one line per hook run
MONO_PORT=19446
mkdir -p "$MONO_BASE" "$MONO_MDIR"

git init -q --bare -b main "$MONO_ORIGIN"
git init -q -b main "$MONO_WORK"
cd "$MONO_WORK" || exit 1
git config user.email smoke@test && git config user.name smoke
for s in a b; do
  mkdir -p "$s/.deploy"
  cat > "$s/docker-compose.yml" <<'EOF'
services:
  app:
    image: alpine:3.20
    command: ["sleep", "300"]
EOF
  # Same hook in both subdirs: keys its marker by $DEPLOY_STACK and records
  # its cwd, which must be the subdir (proves the content root moved).
  cat > "$s/.deploy/pre-deploy.sh" <<EOF
#!/bin/sh
echo "\$DEPLOY_STACK cwd=\$(pwd)" >> "$MONO_MDIR/\$DEPLOY_STACK"
exit 0
EOF
  chmod +x "$s/.deploy/pre-deploy.sh"
done
git add -A && git commit -qm "mono v1"
git remote add origin "$MONO_ORIGIN"
git push -q origin main

cat > "$WS/mono.toml" <<EOF
base_dir = "$MONO_BASE"
poll_interval = "5s"
run_timeout = "5m"
[api]
# A short socket path: the unix-socket staging dir lives under dirname(socket)
# and the whole thing must fit macOS's ~104-char sun_path limit — the deep
# mktemp workspace + "mono-base/" would otherwise overflow it.
socket = "$WS/m.sock"
[health]
listen = "127.0.0.1:$MONO_PORT"
[[stack]]
name = "mono-a"
repo = "file://$MONO_ORIGIN"
path = "a"
verify_timeout = "60s"
[[stack]]
name = "mono-b"
repo = "file://$MONO_ORIGIN"
path = "b"
verify_timeout = "60s"
EOF

"$PLICO" serve --config "$WS/mono.toml" > "$WS/mono.log" 2>&1 &
MONO_PID=$!

i=0
while [ $i -lt 90 ]; do
  n=$(grep -c '"last_status": *"success"' "$MONO_BASE/state.json" 2>/dev/null || echo 0)
  [ "$n" -ge 2 ] && break
  i=$((i+1)); sleep 1
done
[ "${n:-0}" -ge 2 ] \
  && ok "both monorepo stacks deployed from one repo" \
  || fail "monorepo stacks did not both reach success (got ${n:-0}/2)"
# Each hook ran once, with its cwd at its own subdirectory.
grep -q "cwd=.*/a$" "$MONO_MDIR/mono-a" 2>/dev/null \
  && grep -q "cwd=.*/b$" "$MONO_MDIR/mono-b" 2>/dev/null \
  && ok "each stack's hook ran with cwd at its subdirectory" \
  || fail "hook cwd not rooted at the subdirectory"

A_BEFORE=$(wc -l < "$MONO_MDIR/mono-a" 2>/dev/null | tr -d ' ')
B_BEFORE=$(wc -l < "$MONO_MDIR/mono-b" 2>/dev/null | tr -d ' ')

# Commit touching ONLY a/. The repo HEAD moves for both stacks, but only "a"
# has a change under its subtree.
sed -i.bak 's/"300"/"301"/' a/docker-compose.yml && rm -f a/docker-compose.yml.bak
git add -A && git commit -qm "mono v2: change a only" && git push -q origin main

i=0
while [ $i -lt 60 ]; do
  A_NOW=$(wc -l < "$MONO_MDIR/mono-a" 2>/dev/null | tr -d ' ')
  [ "${A_NOW:-0}" -gt "${A_BEFORE:-0}" ] && break
  i=$((i+1)); sleep 1
done
# Give b at least two more poll ticks (5s each) to (wrongly) redeploy if the
# filter leaks.
sleep 12
A_NOW=$(wc -l < "$MONO_MDIR/mono-a" 2>/dev/null | tr -d ' ')
B_NOW=$(wc -l < "$MONO_MDIR/mono-b" 2>/dev/null | tr -d ' ')
[ "${A_NOW:-0}" -gt "${A_BEFORE:-0}" ] \
  && ok "commit under a/ redeployed stack a (hook re-ran)" \
  || fail "stack a did not redeploy on a change to its subtree"
[ "${B_NOW:-0}" -eq "${B_BEFORE:-0}" ] \
  && ok "commit under a/ did NOT redeploy stack b (path-scoped detection)" \
  || fail "stack b redeployed despite no change under its path (b: $B_BEFORE -> $B_NOW)"

# ── verdict ────────────────────────────────────────────────────────────
if [ -z "$FAILURES" ]; then
  echo PASS
  exit 0
fi
echo "FAIL"; printf '%b\n' "$FAILURES"
exit 1

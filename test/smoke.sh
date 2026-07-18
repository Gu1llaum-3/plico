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
WS="$(mktemp -d -t plico-smoke)"
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
  docker compose -p smoke down --timeout 2 >/dev/null 2>&1
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
cd "$WORK"
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
exit 0
EOF
chmod +x .deploy/pre-deploy.sh
git add -A && git commit -qm "v1"
git remote add origin "$ORIGIN"
git push -q origin main
SHA1=$(git rev-parse HEAD)

# ── 2. plico config with sops enabled on the stack ─────────────────────
cat > "$WS/config.toml" <<EOF
base_dir = "$BASE"
poll_interval = "5s"
run_timeout = "5m"

[health]
listen = "127.0.0.1:$PORT"

[[stack]]
name = "smoke"
repo = "file://$ORIGIN"
sops_files = [".deploy/secrets.enc.env"]
verify_timeout = "60s"
EOF

# ── 3. daemon: the age key reaches sops through plico's environment ────
SOPS_AGE_KEY_FILE="$WS/age.key" "$PLICO" serve --config "$WS/config.toml" > "$LOG" 2>&1 &
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
docker compose -p smoke ps --format '{{.Service}} {{.State}}' 2>/dev/null | grep -q "app running" \
  && ok "container app is running" || fail "container not running"

# ── 4. SOPS assertions (F16) ───────────────────────────────────────────
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

# ── 5. gate test: failing pre-deploy hook must block the new revision ──
cd "$WORK"
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

# ── verdict ────────────────────────────────────────────────────────────
if [ -z "$FAILURES" ]; then
  echo PASS
  exit 0
fi
echo "FAIL"; printf '%b\n' "$FAILURES"
exit 1

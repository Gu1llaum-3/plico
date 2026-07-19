#!/bin/sh

set -eu

ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
INSTALL=$ROOT/install.sh
SERVICE=$ROOT/packaging/plico.service
TMP=$(mktemp -d "${TMPDIR:-/tmp}/plico-installer-test.XXXXXX")
cleanup() { rm -rf "$TMP"; }
trap cleanup 0 HUP INT TERM

fail() { echo "FAIL: $1" >&2; exit 1; }
assert_file() { [ -f "$1" ] || fail "missing file: $1"; }
assert_contains() { grep -F -e "$2" "$1" >/dev/null || fail "$1 does not contain: $2"; }
assert_not_contains() {
    if grep -F -e "$2" "$1" >/dev/null; then fail "$1 unexpectedly contains: $2"; fi
}
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}
make_binary() {
    destination=$1
    version=$2
    payload=$3
    cat >"$destination" <<EOF
#!/bin/sh
if [ "\${1:-}" = version ]; then
    echo "plico $version"
else
    echo "$payload"
fi
EOF
    chmod +x "$destination"
}
make_release() {
    version=$1
    binary=$2
    release_dir=$TMP/releases/v$version
    archive_dir=$TMP/archive-$version
    mkdir -p "$release_dir" "$archive_dir"
    cp "$binary" "$archive_dir/plico"
    printf '# release example %s\nbase_dir = "/opt/docker"\n' "$version" >"$archive_dir/config.example.toml"
    tar -czf "$release_dir/plico_${version}_linux_amd64.tar.gz" -C "$archive_dir" plico config.example.toml
    digest=$(sha256 "$release_dir/plico_${version}_linux_amd64.tar.gz")
    printf '%s  %s\n' "$digest" "plico_${version}_linux_amd64.tar.gz" >"$release_dir/checksums.txt"
}

mkdir -p "$TMP/local" "$TMP/fakebin" "$TMP/releases"
make_binary "$TMP/local/plico-v1" 1.2.3 payload-v1
make_binary "$TMP/local/plico-v2" 1.2.4 payload-v2
make_binary "$TMP/local/plico-wrong" 9.9.9 wrong-release
make_binary "$TMP/local/plico-dev" dev-local local-build
cat >"$TMP/local/plico-runtime-bad" <<'EOF'
#!/bin/sh
if [ "${1:-}" = version ]; then
    echo "plico 1.2.5"
    exit 0
fi
exit 1
EOF
chmod +x "$TMP/local/plico-runtime-bad"
printf '#!/bin/sh\necho not-plico\n' >"$TMP/local/not-plico"
chmod +x "$TMP/local/not-plico"
LOCAL_SHA=$(sha256 "$TMP/local/plico-v1")

DESTDIR=$TMP/stage PLICO_OS=linux PLICO_ARCH=amd64 \
    sh "$INSTALL" --binary "$TMP/local/plico-v1" --sha256 "$LOCAL_SHA" --binary-only
assert_file "$TMP/stage/usr/local/bin/plico"
[ "$("$TMP/stage/usr/local/bin/plico" version)" = "plico 1.2.3" ] || fail "installed wrong local binary"

if DESTDIR=$TMP/bad-hash PLICO_OS=linux PLICO_ARCH=amd64 \
    sh "$INSTALL" --binary "$TMP/local/plico-v1" --sha256 bad --binary-only >/dev/null 2>&1; then
    fail "malformed SHA-256 was accepted"
fi
if DESTDIR=$TMP/bad-binary PLICO_OS=linux PLICO_ARCH=amd64 \
    sh "$INSTALL" --binary "$TMP/local/not-plico" --binary-only >/dev/null 2>&1; then
    fail "binary with invalid version output was accepted"
fi
DESTDIR=$TMP/local-dev PLICO_OS=linux PLICO_ARCH=amd64 \
    sh "$INSTALL" --binary "$TMP/local/plico-dev" --binary-only
[ "$("$TMP/local-dev/usr/local/bin/plico" version)" = "plico dev-local" ] || fail "local development binary was rejected"
if DESTDIR=$TMP/bad-semver PLICO_OS=linux PLICO_ARCH=amd64 \
    sh "$INSTALL" --version 01.2.3 --binary-only >/dev/null 2>&1; then
    fail "invalid release SemVer was accepted"
fi

make_release 1.2.3 "$TMP/local/plico-v1"
make_release 1.2.4 "$TMP/local/plico-wrong"
cat >"$TMP/fakebin/curl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$CURL_LOG"
output=
url=
while [ "$#" -gt 0 ]; do
    case $1 in
        -o) output=$2; shift 2 ;;
        --output) output=$2; shift 2 ;;
        -w|--write-out) shift 2 ;;
        --proto|--proto-redir|--retry|--retry-delay|--connect-timeout) shift 2 ;;
        --tlsv1.2|--retry-all-errors|-fsSL|-fsSLI) shift ;;
        -*) shift ;;
        *) url=$1; shift ;;
    esac
done
case $url in
    */releases/latest) printf 'https://fixtures/releases/tag/v1.2.3' ;;
    *) cp "$FIXTURE_ROOT/${url##*/}" "$output" ;;
esac
EOF
chmod +x "$TMP/fakebin/curl"
: >"$TMP/curl.log"

printf 'user config\n' >"$TMP/config.toml"
printf 'TOKEN=initial\n' >"$TMP/plico.env"
PATH="$TMP/fakebin:$PATH" CURL_LOG="$TMP/curl.log" FIXTURE_ROOT="$TMP/releases/v1.2.3" \
    DESTDIR="$TMP/full" PLICO_OS=linux PLICO_ARCH=amd64 \
    PLICO_RELEASE_LATEST_URL=https://fixtures/releases/latest PLICO_RELEASE_BASE_URL=https://fixtures \
    sh "$INSTALL" --config "$TMP/config.toml" --env-file "$TMP/plico.env"

assert_file "$TMP/full/usr/local/bin/plico"
assert_file "$TMP/full/etc/plico/config.toml"
assert_file "$TMP/full/etc/plico/config.toml.example"
assert_file "$TMP/full/etc/plico/plico.env"
assert_file "$TMP/full/etc/systemd/system/plico.service"
assert_contains "$TMP/full/etc/plico/config.toml.example" '# release example 1.2.3'
assert_contains "$TMP/curl.log" "--proto =https"
assert_contains "$TMP/curl.log" "--retry 3"
cmp "$SERVICE" "$TMP/full/etc/systemd/system/plico.service" >/dev/null || fail "generated service differs from packaging/plico.service"
assert_contains "$TMP/full/etc/systemd/system/plico.service" 'UMask=0022'
[ -d "$TMP/full/var/lib/plico" ] || fail "state directory not created"
[ -d "$TMP/full/opt/docker" ] || fail "docker directory not created"

printf 'keep config\n' >"$TMP/full/etc/plico/config.toml"
printf 'keep example\n' >"$TMP/full/etc/plico/config.toml.example"
printf 'KEEP=1\n' >"$TMP/full/etc/plico/plico.env"
PATH="$TMP/fakebin:$PATH" CURL_LOG="$TMP/curl.log" FIXTURE_ROOT="$TMP/releases/v1.2.3" \
    DESTDIR="$TMP/full" PLICO_OS=linux PLICO_ARCH=amd64 PLICO_RELEASE_BASE_URL=https://fixtures \
    sh "$INSTALL" --version 1.2.3 --config "$TMP/config.toml" --env-file "$TMP/plico.env"
[ "$(cat "$TMP/full/etc/plico/config.toml")" = "keep config" ] || fail "existing config was overwritten"
assert_contains "$TMP/full/etc/plico/config.toml.example" '# release example 1.2.3'
[ "$(cat "$TMP/full/etc/plico/plico.env")" = "KEEP=1" ] || fail "existing environment was overwritten"

if PATH="$TMP/fakebin:$PATH" CURL_LOG="$TMP/curl.log" FIXTURE_ROOT="$TMP/releases/v1.2.4" \
    DESTDIR="$TMP/mismatch" PLICO_OS=linux PLICO_ARCH=amd64 PLICO_RELEASE_BASE_URL=https://fixtures \
    sh "$INSTALL" --version 1.2.4 --binary-only >/dev/null 2>&1; then
    fail "release containing a mismatched binary version was accepted"
fi
[ ! -e "$TMP/mismatch/usr/local/bin/plico" ] || fail "mismatched release binary was installed"
set -- "$TMP/mismatch/usr/local/bin"/.plico.*
[ ! -e "$1" ] || fail "mismatched release left a staged binary behind"

DESTDIR=$TMP/darwin PLICO_OS=darwin PLICO_ARCH=arm64 \
    sh "$INSTALL" --binary "$TMP/local/plico-v1"
assert_file "$TMP/darwin/usr/local/bin/plico"
[ ! -e "$TMP/darwin/etc/systemd/system/plico.service" ] || fail "systemd service installed on Darwin"

# Every host command and destination below is redirected to the fixture.
mkdir -p "$TMP/host/bin" "$TMP/host/state" "$TMP/host/root" "$TMP/host/systemd-runtime"
cat >"$TMP/host/bin/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = -u ] && [ "$#" -eq 1 ]; then printf '1000\n'; exit 0; fi
if [ "${1:-}" = -u ] && [ "${2:-}" = plico ]; then exit 1; fi
exit 0
EOF
cat >"$TMP/host/bin/getent" <<'EOF'
#!/bin/sh
[ "${2:-}" = docker ]
EOF
cat >"$TMP/host/bin/record" <<'EOF'
#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >>"$COMMAND_LOG"
EOF
cat >"$TMP/host/bin/systemctl" <<'EOF'
#!/bin/sh
case $1 in
    is-active) [ -f "$SERVICE_STATE/active" ] ;;
    is-enabled) [ -f "$SERVICE_STATE/enabled" ] ;;
    show) printf '1234\n' ;;
    start) : >"$SERVICE_STATE/active"; printf '%s %s\n' systemctl "$*" >>"$COMMAND_LOG" ;;
    restart)
        printf '%s %s\n' systemctl "$*" >>"$COMMAND_LOG"
        if [ -f "$SERVICE_STATE/fail-restart" ]; then rm -f "$SERVICE_STATE/fail-restart"; exit 1; fi
        : >"$SERVICE_STATE/active"
        ;;
    enable) : >"$SERVICE_STATE/enabled"; printf '%s %s\n' systemctl "$*" >>"$COMMAND_LOG" ;;
    *) printf '%s %s\n' systemctl "$*" >>"$COMMAND_LOG" ;;
esac
EOF
chmod +x "$TMP/host/bin/id" "$TMP/host/bin/getent" "$TMP/host/bin/record" "$TMP/host/bin/systemctl"
for command in groupadd useradd usermod chown stat; do cp "$TMP/host/bin/record" "$TMP/host/bin/$command"; done
: >"$TMP/host/commands.log"

host_install() {
    binary=$1
    PATH="$TMP/host/bin:$PATH" COMMAND_LOG="$TMP/host/commands.log" SERVICE_STATE="$TMP/host/state" \
        PLICO_ALLOW_NON_ROOT=1 PLICO_OS=linux PLICO_ARCH=amd64 \
        PLICO_BINDIR="$TMP/host/root/bin" PLICO_SYSCONFDIR="$TMP/host/root/etc" \
        PLICO_STATEDIR="$TMP/host/root/state" PLICO_DOCKERDIR="$TMP/host/root/docker" \
        PLICO_SYSTEMD_DIR="$TMP/host/root/systemd" PLICO_SYSTEMD_RUNTIME="$TMP/host/systemd-runtime" \
        sh "$INSTALL" --binary "$binary" --config "$TMP/config.toml" --operator alice
}

host_install "$TMP/local/plico-v1"
assert_contains "$TMP/host/root/etc/plico.env" '# SOPS_AGE_KEY_FILE=/etc/plico/age.key'
assert_contains "$TMP/host/commands.log" 'usermod -a -G docker plico'
assert_contains "$TMP/host/commands.log" 'usermod -a -G plico alice'
assert_contains "$TMP/host/commands.log" "chown plico:plico $TMP/host/root/state"
assert_contains "$TMP/host/commands.log" 'systemctl enable plico.service'
assert_contains "$TMP/host/commands.log" 'systemctl start plico.service'

: >"$TMP/host/commands.log"
host_install "$TMP/local/plico-v1"
assert_not_contains "$TMP/host/commands.log" 'systemctl restart plico.service'
assert_not_contains "$TMP/host/commands.log" 'systemctl daemon-reload'

: >"$TMP/host/state/fail-restart"
: >"$TMP/host/commands.log"
if host_install "$TMP/local/plico-v2" >/dev/null 2>&1; then
    fail "failed restart did not fail the installer"
fi
[ "$("$TMP/host/root/bin/plico" version)" = "plico 1.2.3" ] || fail "previous binary was not restored"
assert_contains "$TMP/host/commands.log" 'systemctl restart plico.service'

: >"$TMP/host/commands.log"
if PLICO_VERIFY_ATTEMPTS=1 host_install "$TMP/local/plico-runtime-bad" >/dev/null 2>&1; then
    fail "runtime readiness failure did not fail the installer"
fi
[ "$("$TMP/host/root/bin/plico" version)" = "plico 1.2.3" ] || fail "runtime readiness failure did not restore the previous binary"

rm -f "$TMP/host/state/active" "$TMP/host/state/enabled"
: >"$TMP/host/commands.log"
host_install "$TMP/local/plico-v2"
assert_not_contains "$TMP/host/commands.log" 'systemctl enable plico.service'
assert_not_contains "$TMP/host/commands.log" 'systemctl start plico.service'
[ "$("$TMP/host/root/bin/plico" version)" = "plico 1.2.4" ] || fail "inactive service binary was not upgraded"

: >"$TMP/host/commands.log"
rm -rf "$TMP/host/no-config"
rm -f "$TMP/host/state/active" "$TMP/host/state/enabled"
PATH="$TMP/host/bin:$PATH" COMMAND_LOG="$TMP/host/commands.log" SERVICE_STATE="$TMP/host/state" \
    PLICO_ALLOW_NON_ROOT=1 PLICO_OS=linux PLICO_ARCH=amd64 \
    PLICO_BINDIR="$TMP/host/no-config/bin" PLICO_SYSCONFDIR="$TMP/host/no-config/etc" \
    PLICO_STATEDIR="$TMP/host/no-config/state" PLICO_DOCKERDIR="$TMP/host/no-config/docker" \
    PLICO_SYSTEMD_DIR="$TMP/host/no-config/systemd" PLICO_SYSTEMD_RUNTIME="$TMP/host/systemd-runtime" \
    sh "$INSTALL" --binary "$TMP/local/plico-v1"
assert_not_contains "$TMP/host/commands.log" 'systemctl enable plico.service'
assert_not_contains "$TMP/host/commands.log" 'systemctl start plico.service'

: >"$TMP/host/commands.log"
PATH="$TMP/host/bin:$PATH" COMMAND_LOG="$TMP/host/commands.log" SERVICE_STATE="$TMP/host/state" \
    PLICO_ALLOW_NON_ROOT=1 PLICO_OS=linux PLICO_ARCH=amd64 \
    PLICO_BINDIR="$TMP/host/no-config/bin" PLICO_SYSCONFDIR="$TMP/host/no-config/etc" \
    PLICO_STATEDIR="$TMP/host/no-config/state" PLICO_DOCKERDIR="$TMP/host/no-config/docker" \
    PLICO_SYSTEMD_DIR="$TMP/host/no-config/systemd" PLICO_SYSTEMD_RUNTIME="$TMP/host/systemd-runtime" \
    sh "$INSTALL" --binary "$TMP/local/plico-v1" --config "$TMP/config.toml"
assert_contains "$TMP/host/commands.log" 'systemctl enable plico.service'
assert_contains "$TMP/host/commands.log" 'systemctl start plico.service'
assert_not_contains "$TMP/host/commands.log" 'systemctl restart plico.service'

echo "Installer tests passed"

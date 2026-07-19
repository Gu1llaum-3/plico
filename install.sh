#!/bin/sh

set -eu

REPOSITORY=${PLICO_REPOSITORY:-Gu1llaum-3/plico}
BINDIR=${PLICO_BINDIR:-/usr/local/bin}
SYSCONFDIR=${PLICO_SYSCONFDIR:-/etc/plico}
STATEDIR=${PLICO_STATEDIR:-/var/lib/plico}
DOCKERDIR=${PLICO_DOCKERDIR:-/opt/docker}
SYSTEMD_DIR=${PLICO_SYSTEMD_DIR:-/etc/systemd/system}
SYSTEMD_RUNTIME=${PLICO_SYSTEMD_RUNTIME:-/run/systemd/system}
DOCKER_SOCKET=${PLICO_DOCKER_SOCKET:-/var/run/docker.sock}
SYSTEMCTL=${PLICO_SYSTEMCTL:-systemctl}
GETENT=${PLICO_GETENT:-getent}
GROUPADD=${PLICO_GROUPADD:-groupadd}
USERADD=${PLICO_USERADD:-useradd}
USERMOD=${PLICO_USERMOD:-usermod}
CHOWN=${PLICO_CHOWN:-chown}
STAT=${PLICO_STAT:-stat}

VERSION=
BINARY=
SHA256=
CONFIG=
ENV_FILE=
OPERATOR=
BINARY_ONLY=0
NO_START=0
VERIFY_ATTEMPTS=${PLICO_VERIFY_ATTEMPTS:-15}
STABLE_POLLS=${PLICO_STABLE_POLLS:-5}

usage() {
    cat <<'EOF'
Usage: install.sh [options]

  --version VERSION  Install a SemVer release (default: latest stable)
  --binary PATH      Install a local plico binary instead of a release
  --sha256 HASH      Expected SHA-256 of the local binary or release archive
  --binary-only      Install only /usr/local/bin/plico
  --config PATH      Seed /etc/plico/config.toml if it does not exist
  --env-file PATH    Seed /etc/plico/plico.env if it does not exist
  --operator USER    Add an operator to the plico group (Linux)
  --no-start         Do not start or restart plico
  -h, --help         Show this help

DESTDIR stages filesystem changes without users, ownership, or systemd calls.
PLICO_* path and command overrides are available for package builders/tests.
EOF
}

need_arg() {
    if [ "$#" -lt 2 ] || [ -z "$2" ]; then
        echo "install.sh: $1 requires an argument" >&2
        exit 2
    fi
}

while [ "$#" -gt 0 ]; do
    case $1 in
        --version) need_arg "$@"; VERSION=$2; shift 2 ;;
        --binary) need_arg "$@"; BINARY=$2; shift 2 ;;
        --sha256) need_arg "$@"; SHA256=$2; shift 2 ;;
        --binary-only) BINARY_ONLY=1; shift ;;
        --config) need_arg "$@"; CONFIG=$2; shift 2 ;;
        --env-file) need_arg "$@"; ENV_FILE=$2; shift 2 ;;
        --operator) need_arg "$@"; OPERATOR=$2; shift 2 ;;
        --no-start) NO_START=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "install.sh: unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

[ -z "$BINARY" ] || [ -z "$VERSION" ] || {
    echo "install.sh: --binary and --version cannot be used together" >&2
    exit 2
}
[ -z "$CONFIG" ] || [ -f "$CONFIG" ] || {
    echo "install.sh: config is not a regular file: $CONFIG" >&2
    exit 1
}
[ -z "$ENV_FILE" ] || [ -f "$ENV_FILE" ] || {
    echo "install.sh: environment file is not a regular file: $ENV_FILE" >&2
    exit 1
}

detect_os() {
    value=${PLICO_OS:-$(uname -s)}
    case $value in
        Linux|linux) echo linux ;;
        Darwin|darwin) echo darwin ;;
        FreeBSD|freebsd) echo freebsd ;;
        *) echo "install.sh: unsupported operating system: $value" >&2; exit 1 ;;
    esac
}

detect_arch() {
    value=${PLICO_ARCH:-$(uname -m)}
    case $value in
        x86_64|amd64) echo amd64 ;;
        arm64|aarch64) echo arm64 ;;
        *) echo "install.sh: unsupported architecture: $value" >&2; exit 1 ;;
    esac
}

# Print a canonical SemVer without a leading v, or fail. This implements the
# numeric-leading-zero and identifier rules that a filename-only check misses.
semver() {
    candidate=${1#v}
    printf '%s\n' "$candidate" | awk '
        function ids(s, prerelease, a, n, i) {
            if (s == "") return 0
            n = split(s, a, ".")
            for (i = 1; i <= n; i++) {
                if (a[i] == "" || a[i] !~ /^[0-9A-Za-z-]+$/) return 0
                if (prerelease && a[i] ~ /^[0-9]+$/ && length(a[i]) > 1 && substr(a[i], 1, 1) == "0") return 0
            }
            return 1
        }
        {
            if (NR != 1 || NF != 1) exit 1
            value = $0
            n = split(value, plus, "+")
            if (n > 2 || (n == 2 && !ids(plus[2], 0))) exit 1
            value = plus[1]
            dash = index(value, "-")
            if (dash) {
                pre = substr(value, dash + 1)
                value = substr(value, 1, dash - 1)
                if (!ids(pre, 1)) exit 1
            }
            n = split(value, core, ".")
            if (n != 3) exit 1
            for (i = 1; i <= 3; i++) {
                if (core[i] !~ /^[0-9]+$/ || (length(core[i]) > 1 && substr(core[i], 1, 1) == "0")) exit 1
            }
            print $0
        }
    '
}

valid_hash() {
    printf '%s\n' "$1" | awk 'NR == 1 && length($0) == 64 && $0 ~ /^[0-9A-Fa-f]+$/ { ok = 1 } END { exit !ok }'
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    else
        echo "install.sh: sha256sum, shasum, or openssl is required" >&2
        exit 1
    fi
}

verify_sha256() {
    valid_hash "$1" || { echo "install.sh: invalid SHA-256: $1" >&2; exit 1; }
    expected=$(printf '%s' "$1" | tr 'A-F' 'a-f')
    actual=$(sha256_file "$2" | tr 'A-F' 'a-f')
    valid_hash "$actual" || { echo "install.sh: SHA-256 tool returned an invalid digest" >&2; exit 1; }
    [ "$actual" = "$expected" ] || {
        echo "install.sh: SHA-256 mismatch for $2" >&2
        echo "  expected: $expected" >&2
        echo "  actual:   $actual" >&2
        exit 1
    }
}

download() {
    url=$1
    output=$2
    case $url in https://*) ;; *) echo "install.sh: refusing non-HTTPS download: $url" >&2; exit 1 ;; esac
    if command -v curl >/dev/null 2>&1; then
        curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
            --retry 3 --retry-delay 1 --retry-all-errors --connect-timeout 10 \
            -fsSL -o "$output" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget --https-only --tries=4 --timeout=30 -q -O "$output" "$url"
    else
        echo "install.sh: curl or wget is required" >&2
        exit 1
    fi
}

latest_version() {
    latest_url=${PLICO_RELEASE_LATEST_URL:-https://github.com/$REPOSITORY/releases/latest}
    case $latest_url in https://*) ;; *) echo "install.sh: refusing non-HTTPS latest URL: $latest_url" >&2; exit 1 ;; esac
    command -v curl >/dev/null 2>&1 || {
        echo "install.sh: curl is required to resolve the latest release; use --version with wget" >&2
        exit 1
    }
    resolved=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --retry 3 --retry-delay 1 --retry-all-errors --connect-timeout 10 \
        -fsSLI -o /dev/null -w '%{url_effective}' "$latest_url")
    case $resolved in https://*/releases/tag/*) ;; *) echo "install.sh: unexpected latest release redirect: $resolved" >&2; exit 1 ;; esac
    latest_tag=${resolved##*/}
    latest_semver=$(semver "$latest_tag") || {
        echo "install.sh: latest release tag is not SemVer: $latest_tag" >&2
        exit 1
    }
    case $latest_semver in *-*) echo "install.sh: latest release is not stable: $latest_tag" >&2; exit 1 ;; esac
    printf '%s\n' "$latest_tag"
}

validate_binary() {
    candidate_binary=$1
    if ! version_output=$("$candidate_binary" version 2>/dev/null); then
        echo "install.sh: staged binary cannot run 'version' on this host" >&2
        exit 1
    fi
    binary_version=$(printf '%s\n' "$version_output" | awk 'NR == 1 && NF == 2 && $1 == "plico" { value = $2; next } { bad = 1 } END { if (!bad && value != "") print value }')
    [ -n "$binary_version" ] || {
        echo "install.sh: staged binary returned an invalid version response" >&2
        exit 1
    }
	if [ -z "$EXPECTED_VERSION" ]; then
		return
	fi
	canonical_binary_version=$(semver "$binary_version") || {
		echo "install.sh: staged release version is not SemVer: $binary_version" >&2
		exit 1
	}
	if [ "$canonical_binary_version" != "$EXPECTED_VERSION" ]; then
        echo "install.sh: staged binary version $canonical_binary_version does not match release $EXPECTED_VERSION" >&2
        exit 1
    fi
}

OS=$(detect_os)
ARCH=$(detect_arch)
DESTDIR=${DESTDIR:-}
EXPECTED_VERSION=

case $BINDIR$SYSCONFDIR$STATEDIR$DOCKERDIR$SYSTEMD_DIR in
    *..*) echo "install.sh: installation paths must not contain '..'" >&2; exit 1 ;;
esac
if [ -n "$SHA256" ]; then
    valid_hash "$SHA256" || { echo "install.sh: invalid SHA-256: $SHA256" >&2; exit 2; }
fi
if [ -z "$DESTDIR" ] && [ "${PLICO_ALLOW_NON_ROOT:-0}" != 1 ] && [ "$(id -u)" -ne 0 ]; then
    echo "install.sh: run as root or use DESTDIR" >&2
    exit 1
fi
if [ "$OS" = linux ] && [ "$BINARY_ONLY" -eq 0 ] && [ -z "$DESTDIR" ]; then
    if ! command -v "$SYSTEMCTL" >/dev/null 2>&1 || [ ! -d "$SYSTEMD_RUNTIME" ]; then
        echo "install.sh: active systemd not detected; use --binary-only" >&2
        exit 1
    fi
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/plico-install.XXXXXX")
binary_tmp=
service_tmp=
cleanup() {
    [ -z "$binary_tmp" ] || rm -f "$binary_tmp"
    [ -z "$service_tmp" ] || rm -f "$service_tmp"
    rm -rf "$WORK"
}
trap cleanup 0 HUP INT TERM

default_config=$WORK/config.toml.example
cat >"$default_config" <<'EOF'
# Copy to config.toml and adapt before starting plico.
base_dir = "/opt/docker"
state_file = "/var/lib/plico/state.json"
poll_interval = "60s"
run_timeout = "30m"

[api]
socket = "/run/plico/plico.sock"

[[stack]]
name = "example"
repo = "https://example.invalid/owner/deploy.git"
EOF
default_env=$WORK/plico.env
cat >"$default_env" <<'EOF'
# Optional environment variables referenced by config.toml.
# SOPS_AGE_KEY_FILE=/etc/plico/age.key
# PLICO_NTFY_TOKEN=
EOF

source_binary=
release_config=
if [ -n "$BINARY" ]; then
    [ -f "$BINARY" ] || { echo "install.sh: binary is not a regular file: $BINARY" >&2; exit 1; }
    source_binary=$BINARY
    [ -z "$SHA256" ] || verify_sha256 "$SHA256" "$source_binary"
else
    if [ -n "$VERSION" ]; then
        EXPECTED_VERSION=$(semver "$VERSION") || { echo "install.sh: release version is not SemVer: $VERSION" >&2; exit 2; }
        tag=v$EXPECTED_VERSION
    else
        tag=$(latest_version)
        EXPECTED_VERSION=$(semver "$tag")
    fi
    archive_name=plico_${EXPECTED_VERSION}_${OS}_${ARCH}.tar.gz
    release_base=${PLICO_RELEASE_BASE_URL:-https://github.com/$REPOSITORY/releases/download}
    archive=$WORK/$archive_name
    checksums=$WORK/checksums.txt
    download "$release_base/$tag/$archive_name" "$archive"
    download "$release_base/$tag/checksums.txt" "$checksums"
    expected=$(awk -v name="$archive_name" '$2 == name || $2 == "*" name { print $1; exit }' "$checksums")
    [ -n "$expected" ] || { echo "install.sh: $archive_name is absent from checksums.txt" >&2; exit 1; }
    verify_sha256 "$expected" "$archive"
    [ -z "$SHA256" ] || verify_sha256 "$SHA256" "$archive"
    mkdir "$WORK/archive"
    # Named extraction prevents unexpected archive members from reaching disk.
    tar -xzf "$archive" -C "$WORK/archive" plico config.example.toml
    if [ ! -f "$WORK/archive/plico" ] || [ -L "$WORK/archive/plico" ]; then
        echo "install.sh: release archive does not contain a regular plico binary" >&2
        exit 1
    fi
    source_binary=$WORK/archive/plico
    if [ -e "$WORK/archive/config.example.toml" ]; then
        if [ ! -f "$WORK/archive/config.example.toml" ] || [ -L "$WORK/archive/config.example.toml" ]; then
            echo "install.sh: release archive contains an unsafe config.example.toml" >&2
            exit 1
        fi
        release_config=$WORK/archive/config.example.toml
    fi
fi

target_bindir=$DESTDIR$BINDIR
target_sysconf=$DESTDIR$SYSCONFDIR
target_state=$DESTDIR$STATEDIR
target_docker=$DESTDIR$DOCKERDIR
target_systemd=$DESTDIR$SYSTEMD_DIR
service_path=$target_systemd/plico.service
target_binary=$target_bindir/plico
pending_marker=$target_sysconf/.installation-pending

# Service state is sampled before the binary or unit is replaced.
service_existed=0
[ ! -f "$service_path" ] || service_existed=1
was_active=0
was_enabled=0
if [ "$BINARY_ONLY" -eq 0 ] && [ "$OS" = linux ] && [ -z "$DESTDIR" ] && [ "$service_existed" -eq 1 ]; then
    if "$SYSTEMCTL" is-active --quiet plico.service >/dev/null 2>&1; then was_active=1; fi
    if "$SYSTEMCTL" is-enabled --quiet plico.service >/dev/null 2>&1; then was_enabled=1; fi
fi

mkdir -p "$target_bindir"
binary_tmp=$(mktemp "$target_bindir/.plico.XXXXXX")
cp "$source_binary" "$binary_tmp"
chmod 0755 "$binary_tmp"
validate_binary "$binary_tmp"
new_digest=$(sha256_file "$binary_tmp")
binary_changed=1
had_previous_binary=0
if [ -f "$target_binary" ] && [ ! -L "$target_binary" ]; then
    old_digest=$(sha256_file "$target_binary")
    if [ "$new_digest" = "$old_digest" ]; then
        binary_changed=0
        rm -f "$binary_tmp"
        binary_tmp=
        echo "plico binary is already current at $target_binary"
    else
        cp -p "$target_binary" "$WORK/plico.previous"
        had_previous_binary=1
    fi
fi
if [ "$binary_changed" -eq 1 ]; then
    mv -f "$binary_tmp" "$target_binary"
    binary_tmp=
    echo "Installed plico binary to $target_binary"
fi

if [ "$BINARY_ONLY" -eq 1 ] || [ "$OS" != linux ]; then
    [ "$OS" = linux ] || echo "System service setup is only available on Linux; binary installation complete."
    exit 0
fi

if [ -z "$DESTDIR" ]; then
    "$GETENT" group plico >/dev/null 2>&1 || "$GROUPADD" --system plico
    id -u plico >/dev/null 2>&1 || "$USERADD" --system --gid plico --home-dir "$STATEDIR" --no-create-home --shell /usr/sbin/nologin plico
    docker_group=
    if [ -S "$DOCKER_SOCKET" ]; then
        docker_group=$($STAT -c %G "$DOCKER_SOCKET" 2>/dev/null || :)
        if [ -z "$docker_group" ] || [ "$docker_group" = UNKNOWN ]; then
            docker_group=$($STAT -c %g "$DOCKER_SOCKET" 2>/dev/null || :)
        fi
    fi
    if [ -z "$docker_group" ] && "$GETENT" group docker >/dev/null 2>&1; then
        docker_group=docker
    fi
    [ -z "$docker_group" ] || "$USERMOD" -a -G "$docker_group" plico
    if [ -n "$OPERATOR" ]; then
        id "$OPERATOR" >/dev/null 2>&1 || { echo "install.sh: operator does not exist: $OPERATOR" >&2; exit 1; }
        "$USERMOD" -a -G plico "$OPERATOR"
    fi
fi

mkdir -p "$target_sysconf" "$target_state" "$target_docker" "$target_systemd"
chmod 0750 "$target_sysconf" "$target_state" "$target_docker"

install_if_absent() {
    source_file=$1
    destination=$2
    mode=$3
    label=$4
    if [ -e "$destination" ]; then
        echo "Preserving existing $label at $destination"
        return
    fi
    temp_file=$(mktemp "$(dirname "$destination")/.plico.XXXXXX")
    cp "$source_file" "$temp_file"
    chmod "$mode" "$temp_file"
    mv "$temp_file" "$destination"
    echo "Installed $label to $destination"
}

example_source=$default_config
[ -z "$CONFIG" ] || example_source=$CONFIG
[ -z "$release_config" ] || example_source=$release_config
install_managed() {
	source_file=$1
	destination=$2
	mode=$3
	label=$4
	if [ -f "$destination" ] && [ "$(sha256_file "$source_file")" = "$(sha256_file "$destination")" ]; then
		echo "$label is already current at $destination"
		return
	fi
	temp_file=$(mktemp "$(dirname "$destination")/.plico.XXXXXX")
	cp "$source_file" "$temp_file"
	chmod "$mode" "$temp_file"
	mv "$temp_file" "$destination"
	echo "Installed $label to $destination"
}

install_managed "$example_source" "$target_sysconf/config.toml.example" 0640 "example config"
[ -z "$CONFIG" ] || install_if_absent "$CONFIG" "$target_sysconf/config.toml" 0640 config
env_source=$default_env
[ -z "$ENV_FILE" ] || env_source=$ENV_FILE
install_if_absent "$env_source" "$target_sysconf/plico.env" 0600 "environment file"
activate_pending=0
if [ -f "$pending_marker" ] && [ -n "$CONFIG" ]; then
    activate_pending=1
fi
if [ ! -f "$target_sysconf/config.toml" ]; then
    : >"$pending_marker"
    chmod 0600 "$pending_marker"
fi

service_tmp=$(mktemp "$target_systemd/.plico.service.XXXXXX")
cat >"$service_tmp" <<EOF
[Unit]
Description=plico GitOps deployer
Documentation=https://github.com/$REPOSITORY
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=simple
User=plico
Group=plico
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
EnvironmentFile=-$SYSCONFDIR/plico.env
ExecStart=$BINDIR/plico serve --config $SYSCONFDIR/config.toml
Restart=on-failure
RestartSec=5s
TimeoutStopSec=infinity
RuntimeDirectory=plico
RuntimeDirectoryMode=0750
StateDirectory=plico
StateDirectoryMode=0750
WorkingDirectory=$DOCKERDIR
UMask=0007

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$service_tmp"
service_changed=1
if [ -f "$service_path" ] && [ "$(sha256_file "$service_tmp")" = "$(sha256_file "$service_path")" ]; then
    service_changed=0
    rm -f "$service_tmp"
    service_tmp=
else
    [ ! -f "$service_path" ] || cp -p "$service_path" "$WORK/plico.service.previous"
    mv -f "$service_tmp" "$service_path"
    service_tmp=
fi

rollback() {
    restored=0
    if [ "$had_previous_binary" -eq 1 ]; then
        restore_tmp=$(mktemp "$target_bindir/.plico.rollback.XXXXXX")
        cp -p "$WORK/plico.previous" "$restore_tmp"
        mv -f "$restore_tmp" "$target_binary"
        restored=1
    fi
    if [ -f "$WORK/plico.service.previous" ]; then
        restore_service=$(mktemp "$target_systemd/.plico.service.rollback.XXXXXX")
        cp -p "$WORK/plico.service.previous" "$restore_service"
        mv -f "$restore_service" "$service_path"
        restored=1
    fi
    if [ "$service_existed" -eq 0 ]; then
        "$SYSTEMCTL" stop plico.service >/dev/null 2>&1 || :
        "$SYSTEMCTL" disable plico.service >/dev/null 2>&1 || :
        "$SYSTEMCTL" daemon-reload >/dev/null 2>&1 || :
        : >"$pending_marker"
        chmod 0600 "$pending_marker"
        echo "install.sh: plico failed to start; service left stopped and disabled" >&2
        exit 1
    fi
    if [ "$restored" -eq 0 ]; then
        "$SYSTEMCTL" stop plico.service >/dev/null 2>&1 || :
        "$SYSTEMCTL" disable plico.service >/dev/null 2>&1 || :
        echo "install.sh: plico failed to start; service left stopped and disabled" >&2
        exit 1
    fi
    "$SYSTEMCTL" daemon-reload >/dev/null 2>&1 || :
    if [ "$was_active" -eq 0 ]; then
        "$SYSTEMCTL" stop plico.service >/dev/null 2>&1 || :
        if [ "$was_enabled" -eq 0 ]; then
            "$SYSTEMCTL" disable plico.service >/dev/null 2>&1 || :
        fi
        echo "install.sh: plico activation failed; restored the previous inactive installation" >&2
        exit 1
    fi
    "$SYSTEMCTL" restart plico.service >/dev/null 2>&1 || :
    echo "install.sh: plico failed to restart; restored the previous installation" >&2
    exit 1
}

verify_service() {
    attempt=0
    stable_pid=
    stable_count=0
    while [ "$attempt" -lt "$VERIFY_ATTEMPTS" ]; do
        if "$SYSTEMCTL" is-active --quiet plico.service >/dev/null 2>&1; then
            if "$target_binary" status --config "$SYSCONFDIR/config.toml" >/dev/null 2>&1; then
                return 0
            fi
            current_pid=$($SYSTEMCTL show --property MainPID --value plico.service 2>/dev/null || :)
            if [ -n "$current_pid" ] && [ "$current_pid" != 0 ] && [ "$current_pid" = "$stable_pid" ]; then
                stable_count=$((stable_count + 1))
                if [ "$stable_count" -ge "$STABLE_POLLS" ]; then
                    return 0
                fi
            else
                stable_pid=$current_pid
                stable_count=1
            fi
        else
            stable_pid=
            stable_count=0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done
    return 1
}

if [ -z "$DESTDIR" ]; then
    "$CHOWN" root:plico "$SYSCONFDIR"
    "$CHOWN" plico:plico "$STATEDIR"
    "$CHOWN" plico:plico "$DOCKERDIR"
    for owned_file in config.toml.example config.toml plico.env; do
        [ ! -f "$SYSCONFDIR/$owned_file" ] || "$CHOWN" root:plico "$SYSCONFDIR/$owned_file"
    done
    if [ "$service_changed" -eq 1 ]; then
        "$SYSTEMCTL" daemon-reload
    fi
    if [ "$service_existed" -eq 0 ]; then
        if [ -f "$SYSCONFDIR/config.toml" ]; then
            "$SYSTEMCTL" enable plico.service || rollback
        fi
        if [ "$NO_START" -eq 0 ] && [ -f "$SYSCONFDIR/config.toml" ]; then
            "$SYSTEMCTL" start plico.service || rollback
            verify_service || rollback
        fi
    elif [ "$was_active" -eq 1 ] && [ "$NO_START" -eq 0 ] && { [ "$binary_changed" -eq 1 ] || [ "$service_changed" -eq 1 ]; }; then
        "$SYSTEMCTL" restart plico.service || rollback
        verify_service || rollback
    elif [ "$activate_pending" -eq 1 ] && [ "$was_active" -eq 0 ] && [ "$was_enabled" -eq 0 ]; then
        "$SYSTEMCTL" enable plico.service || rollback
        if [ "$NO_START" -eq 0 ]; then
            "$SYSTEMCTL" start plico.service || rollback
            verify_service || rollback
        fi
        rm -f "$pending_marker"
    fi
    # No enable/disable operation is performed on upgrades: was_enabled is
    # intentionally sampled only to document and preserve that state.
    : "$was_enabled"
fi

if [ ! -f "$target_sysconf/config.toml" ]; then
    echo "No active config installed; edit config.toml.example and copy it to config.toml before starting plico." >&2
fi
echo "Installed plico systemd service to $service_path"

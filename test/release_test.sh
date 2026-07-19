#!/bin/sh

set -eu

DIST=${1:-dist}

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

[ -f "$DIST/checksums.txt" ] || fail "missing $DIST/checksums.txt"

count=0
for os in linux darwin freebsd; do
    for arch in amd64 arm64; do
        set -- "$DIST"/plico_*_"$os"_"$arch".tar.gz
        if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
            fail "expected one archive for $os/$arch"
        fi
        archive=$1
        name=${archive##*/}
        expected=$(awk -v name="$name" '$2 == name || $2 == "*" name { print $1 }' "$DIST/checksums.txt")
        [ -n "$expected" ] || fail "missing checksum for $name"
        [ "$(sha256_file "$archive")" = "$expected" ] || fail "bad checksum for $name"
        contents=$(tar -tzf "$archive")
        for member in plico README.md config.example.toml install.sh packaging/plico.service; do
            printf '%s\n' "$contents" | grep -Fx "$member" >/dev/null || fail "$name misses $member"
        done
        count=$((count + 1))
    done
done

[ "$count" -eq 6 ] || fail "expected six release archives, got $count"
echo "Release artifact tests passed"

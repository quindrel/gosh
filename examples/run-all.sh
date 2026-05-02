#!/bin/sh
# run-all.sh — runs every example in sequence, gating each on its
# required env vars. Examples missing required env are skipped
# with a clear note (rather than failing hard).
#
# Usage:
#   ./examples/run-all.sh
#
# Exits 0 if every attempted example succeeded; non-zero if any
# attempted example failed. Skipped examples don't affect the
# exit code.

set -u

# Required for everything
: "${SH_API_KEY:?SH_API_KEY must be set}"
: "${SH_CLIENT_ID:?SH_CLIENT_ID must be set}"

cd "$(dirname "$0")/.."

failed=0
attempted=0
skipped=0

run() {
    name="$1"
    shift
    # Each remaining arg is a required env var; if any are unset,
    # skip this example.
    for var in "$@"; do
        eval "val=\${$var:-}"
        if [ -z "$val" ]; then
            echo
            echo "─── SKIP: examples/$name (missing $var) ───"
            skipped=$((skipped + 1))
            return
        fi
    done

    echo
    echo "─── RUN: examples/$name ───"
    attempted=$((attempted + 1))
    if go run "./examples/$name"; then
        echo "─── PASS: examples/$name ───"
    else
        echo "─── FAIL: examples/$name ───"
        failed=$((failed + 1))
    fi
}

run server-reads
run dns
run mail            SH_MAIL_SERVER
run cloud-volume    SH_CLOUD_SERVER
run server-snapshot SH_TEST_SERVER
run srs

echo
echo "==================================================="
echo "Examples summary:"
echo "  attempted:  $attempted"
echo "  passed:     $((attempted - failed))"
echo "  failed:     $failed"
echo "  skipped:    $skipped (missing env vars)"
echo "==================================================="

[ "$failed" -eq 0 ]

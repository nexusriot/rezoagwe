#!/usr/bin/env bash
# Hermetic end-to-end run for the cluster.
#
# Builds the real binaries into an image, starts a rendezvous and five nodes on
# a private network with no persistence and no published ports, and runs the Go
# suite from a sixth container. Nothing is installed on the host, nothing
# outlives the run, and the stack comes down whether the tests pass or fail.
source "$(dirname "$0")/common.sh"

need docker "install Docker to run the end-to-end suite"
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"

COMPOSE=(docker compose -f "$ROOT/e2e/docker-compose.yml")
KEEP="${KEEP_STACK:-0}"

# The two clocks the suite has to agree with: when the late joiner arrives and
# when the leaver shuts down. Exported so compose stamps them into both the
# nodes and the test runner, which is the only way the two stay in step.
export E2E_LATE_JOIN_SEC="${E2E_LATE_JOIN_SEC:-25}"
export E2E_LEAVE_SEC="${E2E_LEAVE_SEC:-45}"
export E2E_RESTART_SEC="${E2E_RESTART_SEC:-30}"

cleanup() {
    local status=$?
    if [ "$KEEP" = "1" ]; then
        warn "leaving the stack up (KEEP_STACK=1); tear it down with:"
        warn "  docker compose -f e2e/docker-compose.yml down -v"
        return
    fi
    step "tearing the stack down"
    "${COMPOSE[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
    return $status
}
trap cleanup EXIT

# Every service, not just one of them: `up` only builds an image that is missing,
# so building a single service leaves the rest running whatever was in the cache
# from a previous checkout — a suite that passes against last week's binary is
# worse than no suite at all.
step "building the node image"
"${COMPOSE[@]}" build --quiet

step "starting the cluster (late joiner at ${E2E_LATE_JOIN_SEC}s, leaver until ${E2E_LEAVE_SEC}s, restart at ${E2E_RESTART_SEC}s)"
"${COMPOSE[@]}" up -d --wait --force-recreate node-a node-b node-c

# The timed ones are not waited on: when they should have arrived, and whether
# they came back, is what the suite is there to decide.
"${COMPOSE[@]}" up -d --force-recreate node-late node-leaver node-restart stranger-key stranger-cluster

step "running the end-to-end suite"
set +e
"${COMPOSE[@]}" run --rm --no-deps tests
status=$?
set -e

if [ $status -ne 0 ]; then
    warn "the suite failed — cluster logs follow"
    for svc in bootstrap node-a node-b node-c node-late node-leaver node-restart; do
        warn "--- $svc"
        "${COMPOSE[@]}" logs --no-color --tail 40 "$svc" >&2 || true
    done
    die "end-to-end tests failed"
fi

ok "end-to-end tests passed"

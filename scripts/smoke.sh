#!/bin/sh
# Offline acceptance: real CLI, local bare git, SQLite, fake LLM and httptest GitHub.
set -eu
umask 077
cd "$(dirname "$0")/.."
# Honor explicit cache choices. Use a writable temp cache in restricted worktrees.
smoke_tmp=${TMPDIR:-/tmp}
if [ -d /private/tmp ]; then smoke_tmp=/private/tmp; fi
export GOCACHE=${GOCACHE:-$smoke_tmp/patchbot-smoke-go}
export GOPROXY=off
make build
PATCHBOT_SMOKE_BINARY="$PWD/bin/patchbot" go test -tags smoke -run '^TestSmoke$' -count=1 -v ./internal/runner

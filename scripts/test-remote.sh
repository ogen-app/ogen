#!/usr/bin/env bash
# Run the unit suite (`make test`) on another machine over SSH, so the race
# build and the test Postgres don't load this one.
#
# The working tree, uncommitted changes included, is rsynced to
# ~/src/ogen-<checkout dir> on REMOTE_HOST (default: mac2, an ~/.ssh/config
# alias). Each checkout or worktree gets its own remote directory, container
# name and Postgres port, so suites from several worktrees can run at once.
# coverage.out is copied back, and the exit code is the suite's.
#
# The remote needs Go, make and a running Docker; its login shell must have
# them on PATH.
set -euo pipefail

host=${REMOTE_HOST:-mac2}
root=$(git rev-parse --show-toplevel)
name=$(basename "$root")
dir="src/ogen-$name"
port=$((5433 + $(printf '%s' "$name" | cksum | cut -d' ' -f1) % 500))

# macOS's rsync (2.6.9) has no --mkpath and won't create missing parents.
ssh "$host" "mkdir -p ~/$dir"
rsync -az --delete \
	--exclude .git --exclude .claude/worktrees --exclude coverage.out \
	"$root/" "$host:$dir/"

set +e
# GOPATH/bin, where `make test` installs ginkgo, is often not on the remote PATH.
ssh -t "$host" "zsh -lc 'export PATH=\$(go env GOPATH)/bin:\$PATH; cd ~/$dir && make test PG_TEST_CONTAINER=ogen-test-pg-$name PG_TEST_PORT=$port'"
rc=$?
set -e

rsync -az "$host:$dir/coverage.out" "$root/" 2>/dev/null || true
exit $rc

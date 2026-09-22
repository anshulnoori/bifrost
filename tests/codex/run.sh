#!/usr/bin/env bash
set -euo pipefail
# Run from the repository root after make setup-workspace and npm build in ui.
test -f go.work || { echo 'Run make setup-workspace first.' >&2; exit 1; }
export GOMAXPROCS=${GOMAXPROCS:-2}
bash tests/codex/local_test.sh
go work use ./integrations/headroom
build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
go build -o "$build_dir/bifrost" ./transports/bifrost-http
go build -buildmode=plugin -o "$build_dir/headroom.so" ./integrations/headroom
go test -race ./core/providers/codex ./framework/codex ./framework/encrypt ./framework/modelcatalog/...
go test -race ./transports/bifrost-http/handlers ./transports/bifrost-http/lib -run 'Codex|Headroom'
go test -race ./integrations/headroom/...
go test -race ./plugins/semanticcache -run '^TestCodex'
BIFROST_CODEX_TEST_BINARY="$build_dir/bifrost" BIFROST_CODEX_TEST_PLUGIN="$build_dir/headroom.so" \
  go test -race -v -count=1 ./tests/codex/e2e_test.go

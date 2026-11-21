#!/usr/bin/env bash
# check.sh runs every check a change must pass: the Go build, tests, vet, format, and lint; shellcheck; the
# installer's bats tests; Packer's format and validation; and actionlint on the workflows. It runs in the dev
# container, locally and as the first job of the release workflow (.github/workflows/release.yml), which builds the
# images next. It builds nothing that takes long, and the integration suite needs a Proxmox node.
set -euo pipefail
cd "$(dirname "$0")/../.."

step() { printf '\n== %s\n' "$*"; }

step "go build"
go build ./...
step "go test"
go test -race ./...
step "go vet"
go vet ./...
go vet -tags integration ./...
step "gofmt"
unformatted=$(gofmt -l .)
[[ -z $unformatted ]] || { echo "not formatted: $unformatted" && exit 1; }
step "golangci-lint"
golangci-lint run

step "shellcheck"
shellcheck images/common/*.sh images/*/scripts/*.sh images/*/tests/*.sh \
	test/integration/*.sh test/integration/node/*.sh install/*.sh install/tests/helpers.bash .github/scripts/*.sh
step "bats"
bats install/tests .github/scripts/tests

step "packer"
packer fmt -check -recursive images
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
packer init images/runner >/dev/null
# validate refuses to run when a build's output directory exists, so point it somewhere empty.
packer validate -var output_directory="$out/output-runner" images/runner

step "actionlint"
actionlint

echo
echo "all checks passed"

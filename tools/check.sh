#!/usr/bin/env bash
# Everything that runs before a commit and in CI.
set -euo pipefail
cd "$(dirname "$0")/.."

unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "not formatted:"; echo "$unformatted"; exit 1
fi
go vet ./...
go test -race ./...

# The worker has no dependencies outside the standard library.
if [ -n "$(go list -deps -test -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./... | sort -u)" ]; then
  echo "a dependency appeared; the worker uses the standard library only"; exit 1
fi

# House style: no em or en dashes.
if grep -rnF -e $'\xe2\x80\x93' -e $'\xe2\x80\x94' --exclude-dir=.git . ; then
  echo "em or en dash found"; exit 1
fi

if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./...
fi
echo "check ok"

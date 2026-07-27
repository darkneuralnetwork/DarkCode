#!/usr/bin/env bash
set -e
go build ./...
out=$(go run .)
[ "$out" = "hello, world" ] || { echo "expected 'hello, world', got: $out"; exit 1; }

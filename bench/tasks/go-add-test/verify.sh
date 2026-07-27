#!/usr/bin/env bash
set -e
ls *_test.go >/dev/null 2>&1 || { echo "no test file was created"; exit 1; }
go test ./... >/dev/null
grep -q "Add(" *_test.go || { echo "test does not call Add"; exit 1; }

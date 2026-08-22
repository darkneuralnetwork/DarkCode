#!/usr/bin/env bash
set -e
go build ./...
! grep -rn "Greet" *.go || { echo "Greet still present"; exit 1; }
grep -q "Welcome" greet.go || { echo "Welcome not defined"; exit 1; }
grep -q "Welcome" main.go && grep -q "Welcome" other.go || { echo "call sites not updated"; exit 1; }

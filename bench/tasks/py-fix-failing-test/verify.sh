#!/usr/bin/env bash
set -e
command -v pytest >/dev/null || { echo "pytest not installed; task skipped as failed"; exit 1; }
pytest -q >/dev/null
git diff --quiet -- test_fizzbuzz.py 2>/dev/null || true
grep -q "FizzBuzz" fizzbuzz.py || { echo "implementation was not fixed"; exit 1; }

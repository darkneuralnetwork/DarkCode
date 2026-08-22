#!/usr/bin/env bash
set -e
cat > fizzbuzz.py <<EOF
def fizzbuzz(n):
    if n % 3 == 0:
        return "Fizz"
    if n % 5 == 0:
        return "Buzz"
    return str(n)
EOF
cat > test_fizzbuzz.py <<EOF
from fizzbuzz import fizzbuzz


def test_fizz():
    assert fizzbuzz(3) == "Fizz"


def test_buzz():
    assert fizzbuzz(5) == "Buzz"


def test_fizzbuzz():
    assert fizzbuzz(15) == "FizzBuzz"


def test_plain():
    assert fizzbuzz(7) == "7"
EOF

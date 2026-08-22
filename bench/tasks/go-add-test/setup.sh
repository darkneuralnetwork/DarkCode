#!/usr/bin/env bash
set -e
cat > go.mod <<EOF
module benchtask

go 1.24
EOF
cat > calc.go <<EOF
package calc

// Add returns the sum of a and b.
func Add(a, b int) int { return a + b }
EOF

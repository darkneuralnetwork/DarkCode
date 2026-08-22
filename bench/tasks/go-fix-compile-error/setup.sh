#!/usr/bin/env bash
set -e
cat > go.mod <<EOF
module benchtask

go 1.24
EOF
cat > main.go <<EOF
package main

import "fmt"

func main() {
	msg := greet("world")
	fmt.Println(msg)
}

func greet(name string) string {
	return "hello, " + nam
}
EOF

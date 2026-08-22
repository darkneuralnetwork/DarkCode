#!/usr/bin/env bash
set -e
cat > go.mod <<EOF
module benchtask

go 1.24
EOF
cat > greet.go <<EOF
package main

func Greet(name string) string { return "hi " + name }
EOF
cat > main.go <<EOF
package main

import "fmt"

func main() { fmt.Println(Greet("dark")) }
EOF
cat > other.go <<EOF
package main

func shout(n string) string { return Greet(n) + "!" }
EOF

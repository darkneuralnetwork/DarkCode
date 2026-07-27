// Command benchrun executes a DarkCode benchmark suite and reports the score.
//
//	go run ./bench/cmd/benchrun -tasks bench/tasks -agent ./darkcode
//
// The report is written to stdout, and to a JSON file with -json so results
// can be tracked over time or attached to a release.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/darkcode/bench"
)

func main() {
	tasksDir := flag.String("tasks", "bench/tasks", "directory of task folders")
	agentPath := flag.String("agent", "./darkcode", "path to the agent binary")
	jsonOut := flag.String("json", "", "also write the report as JSON to this path")
	flag.Parse()

	tasks, err := bench.LoadTasks(*tasksDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading tasks: %v\n", err)
		os.Exit(2)
	}
	if len(tasks) == 0 {
		fmt.Fprintf(os.Stderr, "no tasks found in %s\n", *tasksDir)
		os.Exit(2)
	}

	// Ctrl-C reports what has run so far rather than discarding it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("running %d task(s) against %s\n\n", len(tasks), *agentPath)
	report := bench.Run(ctx, tasks, bench.BinaryAgent{Path: *agentPath})
	fmt.Print(report.Format())

	if *jsonOut != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, data, 0o644)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", *jsonOut, err)
			os.Exit(1)
		}
		fmt.Printf("\nreport written to %s\n", *jsonOut)
	}

	// A non-zero exit on any failure lets CI gate on the suite.
	if report.Solved < report.Total {
		os.Exit(1)
	}
}

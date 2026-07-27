package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/darkcode/memory"
)

// handleRollback implements /rollback:
//
//	/rollback              list checkpoints
//	/rollback N            restore the workspace to checkpoint N (0 = newest)
//	/rollback N <file>     restore a single file from checkpoint N
//	/rollback diff N       show what changed since checkpoint N
//
// Restoring also rewinds the conversation to the length recorded in the
// checkpoint, so the agent stops reasoning from turns that describe files
// that no longer exist.
func (c *Console) handleRollback(args []string) {
	if c.ckpt == nil {
		fmt.Println(paint(cYellow, "  checkpoints are unavailable in this session"))
		return
	}

	if len(args) == 0 {
		c.printCheckpoints()
		return
	}

	if args[0] == "diff" {
		n := 0
		if len(args) > 1 {
			n, _ = strconv.Atoi(args[1])
		}
		changes, e, err := c.ckpt.Diff(n)
		if err != nil {
			fmt.Println(paint(cRed, "  ✗ "+err.Error()))
			return
		}
		if len(changes) == 0 {
			fmt.Println(paint(cGray, fmt.Sprintf("  workspace is identical to checkpoint #%d", e.ID)))
			return
		}
		fmt.Println(paint(cWhite, fmt.Sprintf("  since checkpoint #%d (%s):", e.ID, e.Label)))
		for _, ch := range changes {
			fmt.Println("   " + paint(statusColor(ch.Status), fmt.Sprintf("%-9s %s", ch.Status, ch.Path)))
		}
		return
	}

	n, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Println(paint(cYellow, "usage: /rollback [N | diff N | N <file>]"))
		return
	}

	if len(args) > 1 {
		file := strings.Join(args[1:], " ")
		if err := c.ckpt.RollbackFile(n, file); err != nil {
			fmt.Println(paint(cRed, "  ✗ "+err.Error()))
			return
		}
		fmt.Println(paint(cGreen, "  ✓ restored "+file))
		return
	}

	entry, changes, err := c.ckpt.Rollback(n)
	if err != nil {
		fmt.Println(paint(cRed, "  ✗ "+err.Error()))
		return
	}
	for _, ch := range changes {
		fmt.Println("   " + paint(statusColor(ch.Status), fmt.Sprintf("%-9s %s", reverseStatus(ch.Status), ch.Path)))
	}
	c.mem.STMTruncate(entry.Turn)
	fmt.Println(paint(cGreen, fmt.Sprintf("  ✓ rolled back to checkpoint #%d — %d file(s) restored, conversation rewound",
		entry.ID, len(changes))))
}

func (c *Console) printCheckpoints() {
	entries := c.ckpt.List()
	if len(entries) == 0 {
		fmt.Println(paint(cGray, "  no checkpoints yet — one is taken before every file-modifying action"))
		return
	}
	fmt.Println(paint(cWhite, "  checkpoints (newest last):"))
	for _, e := range entries {
		fmt.Printf("   %s %s %s\n",
			paint(cBlue, fmt.Sprintf("#%-3d", e.ID)),
			paint(cGray, e.Time.Format(time.RFC3339)),
			e.Label)
	}
	fmt.Println(paint(cGray, "  /rollback N to restore · /rollback diff N to preview"))
}

// handleHealth prints the repository's structural health: a score, the issue
// counts behind it, and the worst offenders. Answers "what is wrong with this
// codebase" from the graph, without an LLM call.
func (c *Console) handleHealth() {
	kg, ok := c.mem.KG().(*memory.KnowledgeGraph)
	if !ok || kg == nil {
		fmt.Println(paint(cYellow, "  knowledge graph unavailable"))
		return
	}
	rep := kg.Health()
	colour := cGreen
	switch {
	case rep.Score < 50:
		colour = cRed
	case rep.Score < 80:
		colour = cYellow
	}
	fmt.Printf("  %s %s\n", paint(colour, fmt.Sprintf("health %.0f/100", rep.Score)),
		paint(cGray, fmt.Sprintf("· %d files · %d symbols", rep.Files, rep.Symbols)))
	if len(rep.Findings) == 0 {
		fmt.Println(paint(cGray, "  no structural issues found"))
		return
	}
	for kind, n := range rep.Counts {
		fmt.Printf("   %s %s\n", paint(cGray, fmt.Sprintf("%-18s", kind)), paint(cWhite, fmt.Sprintf("%d", n)))
	}
	fmt.Println(paint(cGray, "  worst offenders:"))
	for i, f := range rep.Findings {
		if i == 10 {
			break
		}
		fmt.Printf("   %s %s %s\n", paint(cYellow, "•"), paint(cWhite, f.Subject), paint(cGray, f.Detail))
	}
}

// handleEvolution prints what changed structurally between two commits —
// dependencies, API surface, cycles — rather than lines. Defaults to the last
// commit. Usage: /evolution [from] [to]
func (c *Console) handleEvolution(args []string) {
	from, to := "HEAD~1", "HEAD"
	if len(args) > 0 {
		from = args[0]
	}
	if len(args) > 1 {
		to = args[1]
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Println(paint(cRed, "  ✗ "+err.Error()))
		return
	}
	fmt.Println(paint(cGray, fmt.Sprintf("  diffing structure %s → %s…", from, to)))
	events, err := memory.DiffCommits(cwd, from, to)
	if err != nil {
		fmt.Println(paint(cRed, "  ✗ "+err.Error()))
		return
	}
	if len(events) == 0 {
		fmt.Println(paint(cGray, "  no structural change"))
		return
	}
	for i, e := range events {
		if i == 20 {
			fmt.Println(paint(cGray, fmt.Sprintf("   …and %d more", len(events)-i)))
			break
		}
		colour := cGray
		switch {
		case e.Severity >= 0.9:
			colour = cRed
		case e.Severity >= 0.5:
			colour = cYellow
		}
		fmt.Printf("   %s %s\n", paint(colour, fmt.Sprintf("%-22s", e.Kind)), paint(cWhite, e.Detail))
	}
}

// reverseStatus describes a change from the rollback's point of view: a file
// the working tree "created" is about to be deleted, and vice versa.
func reverseStatus(s string) string {
	switch s {
	case "created":
		return "deleted"
	case "deleted":
		return "restored"
	default:
		return "restored"
	}
}

func statusColor(s string) string {
	switch s {
	case "created":
		return cGreen
	case "deleted":
		return cRed
	default:
		return cYellow
	}
}

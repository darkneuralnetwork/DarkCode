package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/darkcode/core"
)

// handleSession implements /session:
//
//	/session                 summarise the current and past sessions
//	/session export [path]   write the transcript + task history to a file
//	/session prune <days>    drop episodic entries older than N days
//
// A session here is the conversation since the last /new plus the durable task
// history behind it — the two things worth carrying out of the tool.
func (c *Console) handleSession(args []string) {
	if len(args) == 0 {
		c.printSessionSummary()
		return
	}
	switch args[0] {
	case "export":
		path := ""
		if len(args) > 1 {
			path = args[1]
		}
		c.exportSession(path)
	case "prune":
		if len(args) < 2 {
			fmt.Println(paint(cYellow, "usage: /session prune <days>"))
			return
		}
		days, err := strconv.Atoi(args[1])
		if err != nil || days < 0 {
			fmt.Println(paint(cYellow, "days must be a non-negative number"))
			return
		}
		n := c.mem.EpisodicPrune(time.Now().AddDate(0, 0, -days))
		fmt.Println(paint(cGreen, fmt.Sprintf("  ✓ pruned %d episode(s) older than %d day(s)", n, days)))
	default:
		fmt.Println(paint(cYellow, "usage: /session [export [path] | prune <days>]"))
	}
}

func (c *Console) printSessionSummary() {
	msgs := c.mem.STMGet()
	episodes := c.mem.EpisodicGet()
	fmt.Printf("  %s %s\n",
		paint(cWhite, fmt.Sprintf("%d message(s) in the current session", len(msgs))),
		paint(cGray, fmt.Sprintf("· %d task(s) in episodic memory", len(episodes))))

	// Group past tasks by day so the history reads as a timeline.
	byDay := map[string]int{}
	var order []string
	for _, e := range episodes {
		day := e.Timestamp.Format("2006-01-02")
		if byDay[day] == 0 {
			order = append(order, day)
		}
		byDay[day]++
	}
	for i, day := range order {
		if i == 7 {
			fmt.Println(paint(cGray, fmt.Sprintf("   …and %d earlier day(s)", len(order)-i)))
			break
		}
		fmt.Printf("   %s %s\n", paint(cBlue, day), paint(cGray, fmt.Sprintf("%d task(s)", byDay[day])))
	}
	fmt.Println(paint(cGray, "  /session export to save · /session prune <days> to trim"))
}

// exportSession writes the transcript and task history. The extension picks
// the format: .json for machine use, anything else for Markdown.
func (c *Console) exportSession(path string) {
	if path == "" {
		path = fmt.Sprintf("darkcode-session-%s.md", time.Now().Format("20060102-150405"))
	}
	msgs := c.mem.STMGet()
	episodes := c.mem.EpisodicGetRecent(50)

	var data []byte
	var err error
	if strings.HasSuffix(path, ".json") {
		data, err = json.MarshalIndent(map[string]interface{}{
			"exported": time.Now(),
			"messages": msgs,
			"episodes": episodes,
		}, "", "  ")
	} else {
		data = []byte(renderSessionMarkdown(msgs, episodes))
	}
	if err != nil {
		fmt.Println(paint(cRed, "  ✗ "+err.Error()))
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Println(paint(cRed, "  ✗ "+err.Error()))
		return
	}
	fmt.Println(paint(cGreen, fmt.Sprintf("  ✓ exported %d message(s) and %d task(s) → %s",
		len(msgs), len(episodes), path)))
}

func renderSessionMarkdown(msgs []core.Message, episodes []core.EpisodicEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# DarkCode session — %s\n\n", time.Now().Format(time.RFC1123))

	b.WriteString("## Conversation\n\n")
	if len(msgs) == 0 {
		b.WriteString("_No messages in the current session._\n\n")
	}
	for _, m := range msgs {
		text := strings.TrimSpace(m.ContentString())
		if text == "" {
			continue
		}
		role := string(m.Role)
		if role != "" {
			role = strings.ToUpper(role[:1]) + role[1:]
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", role, text)
	}

	b.WriteString("## Task history\n\n")
	if len(episodes) == 0 {
		b.WriteString("_No recorded tasks._\n")
		return b.String()
	}
	for _, e := range episodes {
		fmt.Fprintf(&b, "- **%s** — %s _(%s, %s)_\n",
			e.Timestamp.Format("2006-01-02 15:04"), e.TaskGoal, e.Outcome, e.ModelUsed)
		if e.Summary != "" {
			fmt.Fprintf(&b, "  - %s\n", e.Summary)
		}
	}
	return b.String()
}

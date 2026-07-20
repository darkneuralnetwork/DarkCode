package cli

import (
	"sort"

	"github.com/chzyer/readline"
)

// completeModelNames returns every known model name — the primary
// (c.cfg.Model) plus every key in c.cfg.Models — as dynamic completion
// candidates for "/model <TAB>", "/models remove <TAB>", and
// "/models primary <TAB>". readline's own prefix machinery narrows this
// full set against whatever the user has already typed (the same mechanism
// used for the static PcItem tree), so this doesn't need to filter by the
// in-progress line itself.
func (c *Console) completeModelNames(string) []string {
	var names []string
	if c.cfg.Model != "" {
		names = append(names, c.cfg.Model)
	}
	for k := range c.cfg.Models {
		if k != c.cfg.Model {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

func (c *Console) buildCompleter() *readline.PrefixCompleter {
	return readline.NewPrefixCompleter(
		readline.PcItem("/status"),
		readline.PcItem("/memory"),
		readline.PcItem("/tools",
			readline.PcItem("sources"),
			readline.PcItem("connect",
				readline.PcItem("mcp"),
				readline.PcItem("mcp-http"),
				readline.PcItem("file"),
			),
			readline.PcItem("disconnect"),
			readline.PcItem("remove"),
		),
		readline.PcItem("/skills"),
		readline.PcItem("/episodes"),
		readline.PcItem("/config"),
		readline.PcItem("/log"),
		readline.PcItem("/permissions",
			readline.PcItem("reset"),
		),
		readline.PcItem("/new"),
		readline.PcItem("/ingest"),
		readline.PcItem("/reset"),
		readline.PcItem("/models",
			readline.PcItem("add"),
			readline.PcItem("remove", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("primary", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("test", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("disable", readline.PcItemDynamic(c.completeModelNames)),
			readline.PcItem("enable", readline.PcItemDynamic(c.completeModelNames)),
		),
		readline.PcItem("/model", readline.PcItemDynamic(c.completeModelNames)),
		readline.PcItem("/mode",
			readline.PcItem("single"),
			readline.PcItem("escalation"),
			readline.PcItem("consensus"),
		),
		readline.PcItem("/chatmode",
			readline.PcItem("chat"),
			readline.PcItem("build"),
			readline.PcItem("loop"),
		),
		readline.PcItem("/brain",
			readline.PcItem("auto"),
			readline.PcItem("local"),
			readline.PcItem("cloud"),
		),
		readline.PcItem("/memory-profile",
			readline.PcItem("lean"),
			readline.PcItem("balanced"),
			readline.PcItem("max"),
			readline.PcItem("auto"),
		),
		readline.PcItem("/profile",
			readline.PcItem("auto"),
			readline.PcItem("sequential"),
			readline.PcItem("parallel"),
		),
		readline.PcItem("/local",
			readline.PcItem("force"),
			readline.PcItem("on"),
			readline.PcItem("auto"),
			readline.PcItem("off"),
			readline.PcItem("offload",
				readline.PcItem("on"),
				readline.PcItem("off"),
			),
		),
		readline.PcItem("/safety",
			readline.PcItem("strict"),
			readline.PcItem("normal"),
			readline.PcItem("relaxed"),
		),
		readline.PcItem("/compressor"),
		readline.PcItem("/providers"),
		readline.PcItem("/events"),
		readline.PcItem("/usage"),
		readline.PcItem("/history"),
		readline.PcItem("/stats"),
		readline.PcItem("/know"),
		readline.PcItem("/knowledge"),
		readline.PcItem("/plugins"),
		readline.PcItem("/sandbox"),
		readline.PcItem("/pipeline"),
		readline.PcItem("/help"),
		readline.PcItem("/quit"),
		readline.PcItem("/exit"),
	)
}

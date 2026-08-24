package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/memory/memory"
)

func (c *Console) printSkills() {
	skills := c.mem.ProceduralAll()
	if len(skills) == 0 {
		fmt.Println(paint(cGray, "  no skills stored yet."))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "PROCEDURAL MEMORY"), paint(cGray, "("+fmtNum(len(skills))+" skills)"))
	for _, s := range skills {
		fmt.Printf("  %s  %s  %s  %s\n",
			paint(cYellow, "★"),
			paint(cWhite, padRight(s.Name, 22)),
			paint(cGreen, fmt.Sprintf("%d uses %d%%", s.UseCount, int(s.SuccessRate*100))),
			paint(cGray, strutil.Truncate(s.Description, 36)))
	}
}

func (c *Console) printEpisodes() {
	eps := c.mem.EpisodicGetRecent(10)
	if len(eps) == 0 {
		fmt.Println(paint(cGray, "  no episodic memory yet."))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "RECENT EPISODES"), paint(cGray, "("+fmtNum(len(eps))+")"))
	for _, e := range eps {
		icon := paint(cGreen, "✓")
		if e.Outcome != "success" {
			icon = paint(cRed, "✗")
		}
		fmt.Printf("  %s  %s  %s\n", icon, paint(cGray, fmtTime(e.Timestamp)), strutil.Truncate(e.TaskGoal, 56))
	}
}

func (c *Console) printKnowledge() {
	kg := c.mem.KG()
	if kg == nil {
		fmt.Println(paint(cGray, "  knowledge graph unavailable."))
		return
	}
	nodes, edges := kg.Stats()
	fmt.Printf("%s  %s nodes / %s edges\n",
		paint(cAmber+clrBold, "KNOWLEDGE GRAPH"),
		paint(cOrange, fmtNum(nodes)),
		paint(cBlue, fmtNum(edges)))

	// Show the top concept (word) relations so /know reflects the word-relation
	// layer, not just a stat line. Concepts are linked by co-occurrence
	// (related_to edges weighted by how often they appeared together).
	concepts := kg.FindByType(core.KGNodeConcept)
	if len(concepts) == 0 {
		fmt.Println(paint(cGray, "  no concept relations yet — they build up as tasks run."))
		return
	}
	// Rank concepts by edge count (degree) and show the top few with their
	// strongest relations.
	type conceptDeg struct {
		id  string
		lbl string
		deg int
	}
	var ranked []conceptDeg
	for _, n := range concepts {
		deg := len(kg.GetEdges(n.ID))
		ranked = append(ranked, conceptDeg{n.ID, n.Label, deg})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].deg > ranked[j].deg })
	limit := 8
	if len(ranked) < limit {
		limit = len(ranked)
	}
	fmt.Printf("  %s (top %d by connectivity):\n", paint(cAmber, "concept relations"), limit)
	for i := 0; i < limit; i++ {
		cd := ranked[i]
		relsIface := kg.ConceptRelations(cd.lbl)
		rels, _ := relsIface.([]memory.ConceptRelation)
		// Show up to 3 strongest neighbors.
		var neighbors []string
		for _, r := range rels {
			neighbors = append(neighbors, fmt.Sprintf("%s(%.0f)", r.Label, r.Weight))
		}
		if len(neighbors) > 3 {
			neighbors = neighbors[:3]
		}
		joined := "—"
		if len(neighbors) > 0 {
			joined = strings.Join(neighbors, ", ")
		}
		fmt.Printf("    • %s [%d links] → %s\n", paint(cWhite+clrBold, cd.lbl), cd.deg, paint(cGray, joined))
	}
	fmt.Println(paint(cGray, "  (use /know <word> for a concept's full relations, or the 'memory action=kg' tool)"))
}

// printConceptRelations shows all weighted relations for a concept word.
// Reached via /know <word>. Mirrors the memory tool's kg query action.
func (c *Console) printConceptRelations(word string) {
	kg := c.mem.KG()
	if kg == nil {
		fmt.Println(paint(cGray, "  knowledge graph unavailable."))
		return
	}
	relsIface := kg.ConceptRelations(word)
	rels, _ := relsIface.([]memory.ConceptRelation)
	if len(rels) == 0 {
		fmt.Printf("%s no concept relations found for %q\n", paint(cYellow, "!"), word)
		return
	}
	fmt.Printf("%s  %q → %d related concept(s):\n", paint(cAmber+clrBold, "CONCEPT RELATIONS"), word, len(rels))
	// Sort by weight descending so strongest relations show first.
	sort.Slice(rels, func(i, j int) bool { return rels[i].Weight > rels[j].Weight })
	for _, r := range rels {
		fmt.Printf("  %s %s  %s  %s\n",
			paint(cBlue, "•"), paint(cWhite+clrBold, r.Label),
			paint(cGray, r.Relation), paint(cGray, fmt.Sprintf("(weight: %.0f)", r.Weight)))
	}
}

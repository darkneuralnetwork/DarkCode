package core

// event_meta.go — one table for "what does this event type mean," so the CLI
// and the GUI stop each keeping their own copy.
//
// Before this, the icon shown for a given EventType was a 17-case switch in
// the CLI (surfaces/cli/dashboard.go), the set of types worth listening for
// over SSE was a hardcoded array in the GUI (10-sse.js), and which types
// auto-expand in the GUI's raw event feed was a third, independently
// maintained list (40-events.js). A new EventType constant added below with
// no corresponding entry here is caught by TestEventTypesCoversEveryConstant
// (event_meta_test.go) rather than silently rendering as "•" forever.
//
// This follows the same shape as kernel/verb/verb.go: the table lives here,
// in Go, as the source of truth; the CLI imports it directly (same process);
// the GUI fetches it once at boot over HTTP (see the /api/event-types
// handler in surfaces/server) — the same "server owns the table" pattern
// that already stops the CLI and GUI's verb lists from drifting apart.

// EventMeta describes one EventType for display purposes.
type EventMeta struct {
	Type EventType `json:"type"`
	// Icon is the single-glyph symbol the CLI's live activity feed and
	// dashboard show next to this event type.
	Icon string `json:"icon"`
	// Label is the short human name for this type (e.g. "Plan Updated").
	Label string `json:"label"`
	// Significant marks an event type worth showing expanded by default in
	// the GUI's raw event feed, rather than collapsed behind a chevron.
	Significant bool `json:"significant"`
}

// defaultEventMeta is returned by Lookup for a type with no registry entry —
// the same fallback both surfaces already used independently ("•" in the
// CLI, an unstyled type label in the GUI) before this table existed.
var defaultEventMeta = EventMeta{Icon: "•"}

// EventTypes is every known EventType's display metadata, in the order the
// underlying core.EventType constants are declared in orchestrator_types.go.
var EventTypes = []EventMeta{
	{Type: EventTaskUpdate, Icon: "►", Label: "Task Update"},
	{Type: EventAgentSpawn, Icon: "✦", Label: "Agent Spawn"},
	{Type: EventAgentComplete, Icon: "✓", Label: "Agent Complete"},
	{Type: EventToolExecution, Icon: "⚡", Label: "Tool Execution"},
	{Type: EventModelRoute, Icon: "↓", Label: "Model Route"},
	{Type: EventCompression, Icon: "⊕", Label: "Compression"},
	{Type: EventMemoryStore, Icon: "⟐", Label: "Memory Store"},
	{Type: EventFinalOutput, Icon: "▣", Label: "Final Output", Significant: true},
	{Type: EventError, Icon: "✗", Label: "Error", Significant: true},
	{Type: EventDAGUpdate, Icon: "⬡", Label: "DAG Update"},
	{Type: EventSkillExtract, Icon: "★", Label: "Skill Extract"},
	{Type: EventConsensus, Icon: "⚖", Label: "Consensus"},
	{Type: EventTokenUsage, Icon: "🪙", Label: "Token Usage"},
	{Type: EventFileChange, Icon: "✎", Label: "File Change", Significant: true},
	{Type: EventApproval, Icon: "⚑", Label: "Approval", Significant: true},
	{Type: EventChatQuery, Icon: "»", Label: "Chat Query"},
	{Type: EventChatResponse, Icon: "«", Label: "Chat Response"},
	{Type: EventSyncGUI, Icon: "↻", Label: "Sync GUI"},
	{Type: EventPlanUpdated, Icon: "▤", Label: "Plan Updated", Significant: true},
	{Type: EventWorkflowUpdated, Icon: "⛃", Label: "Workflow Updated", Significant: true},
}

// eventMetaIndex is built once for O(1) Lookup.
var eventMetaIndex = func() map[EventType]EventMeta {
	m := make(map[EventType]EventMeta, len(EventTypes))
	for _, em := range EventTypes {
		m[em.Type] = em
	}
	return m
}()

// Lookup returns the display metadata for t, or defaultEventMeta if t has no
// registry entry.
func Lookup(t EventType) EventMeta {
	if em, ok := eventMetaIndex[t]; ok {
		return em
	}
	return defaultEventMeta
}

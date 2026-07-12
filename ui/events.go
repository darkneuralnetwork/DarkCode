package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// OutputMode controls where events go.
type OutputMode int

const (
	OutputNone   OutputMode = iota // events silently consumed
	OutputStderr                   // events go to stderr (CLI mode)
	OutputSSE                      // events broadcast to SSE clients (web mode)
	OutputBoth                     // events go to stderr AND SSE
)

// EventEmitter manages structured UI events.
// In CLI mode (--ui): events go to stderr, final answer goes to stdout.
// In web mode (--serve): events broadcast via SSE channel to browser clients.
type EventEmitter struct {
	mu          sync.Mutex
	mode        OutputMode
	writer      io.Writer
	handlers    []EventHandler
	history     []core.UIEvent
	subscribers map[int]chan core.UIEvent // per-SSE-client channels (F2 fix)
	nextSubID   int
}

// maxHistory bounds the retained event history so a long-running GUI
// session (which emits per-LLM-call token_usage events, plus task/tool
// events indefinitely) cannot grow memory without limit. The SSE clients
// and GET /api/events/history only ever need a recent window.
const maxHistory = 1000

// EventHandler is a callback invoked for each emitted event.
type EventHandler func(event core.UIEvent)

// NewEventEmitter creates a new emitter with the given output mode.
func NewEventEmitter(enabled bool, writer io.Writer) *EventEmitter {
	mode := OutputNone
	if enabled {
		mode = OutputStderr
	}
	return &EventEmitter{
		mode:        mode,
		writer:      writer,
		subscribers: make(map[int]chan core.UIEvent),
	}
}

// NewSSEEventEmitter creates an emitter for web/SSE mode.
// Events go to the broadcast channel for SSE clients.
func NewSSEEventEmitter() *EventEmitter {
	return &EventEmitter{
		mode:        OutputSSE,
		writer:      os.Stderr,
		subscribers: make(map[int]chan core.UIEvent),
	}
}

// NewDualEventEmitter creates an emitter that writes to both stderr and SSE.
func NewDualEventEmitter() *EventEmitter {
	return &EventEmitter{
		mode:        OutputBoth,
		writer:      os.Stderr,
		subscribers: make(map[int]chan core.UIEvent),
	}
}

// Enable turns on UI event streaming.
func (e *EventEmitter) Enable() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mode == OutputNone {
		e.mode = OutputStderr
	}
}

// Disable turns off UI event streaming.
func (e *EventEmitter) Disable() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = OutputNone
}

// IsEnabled returns whether UI streaming is active.
func (e *EventEmitter) IsEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode != OutputNone
}

// SetMode sets the output mode.
func (e *EventEmitter) SetMode(mode OutputMode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = mode
}

// OnHandler registers a callback for each emitted event.
func (e *EventEmitter) OnHandler(handler EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers = append(e.handlers, handler)
}

// RemoveHandler removes a previously-registered callback so dashboards can
// detach cleanly when they close.
func (e *EventEmitter) RemoveHandler(target EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.handlers[:0]
	for _, h := range e.handlers {
		if !funcEqual(h, target) {
			out = append(out, h)
		}
	}
	e.handlers = out
}

// funcEqual compares two EventHandler values by their underlying code pointer.
func funcEqual(a, b EventHandler) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// Subscribe registers a new SSE subscriber and returns its dedicated event
// channel plus an unsubscribe function. Each subscriber gets its own buffered
// channel so multiple browser tabs no longer compete for a single shared
// channel (previously only one of N clients received each event).
func (e *EventEmitter) Subscribe() (<-chan core.UIEvent, func()) {
	e.mu.Lock()
	id := e.nextSubID
	e.nextSubID++
	ch := make(chan core.UIEvent, 256)
	e.subscribers[id] = ch
	e.mu.Unlock()
	unsubscribe := func() {
		e.mu.Lock()
		if c, ok := e.subscribers[id]; ok {
			delete(e.subscribers, id)
			close(c)
		}
		e.mu.Unlock()
	}
	return ch, unsubscribe
}

// Broadcast is retained for backwards compatibility; it returns a new
// subscriber channel (callers should prefer Subscribe and remember to
// unsubscribe).
func (e *EventEmitter) Broadcast() <-chan core.UIEvent {
	ch, _ := e.Subscribe()
	return ch
}

// SubscriberCount returns the number of active SSE subscribers. The server
// uses this to detect when the GUI browser has closed (the last SSE
// connection drops) so it can resume CLI mode — see Server.onSSEDisconnect.
func (e *EventEmitter) SubscriberCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.subscribers)
}

// Emit sends a structured UI event.
func (e *EventEmitter) Emit(eventType core.EventType, content interface{}, opts ...EventOpt) {
	e.mu.Lock()

	event := core.UIEvent{
		Type:      eventType,
		Content:   content,
		Timestamp: time.Now(),
	}

	for _, opt := range opts {
		opt(&event)
	}

	e.history = append(e.history, event)
	// Bound retained history so long-running sessions don't leak memory.
	// Copy the most recent maxHistory entries to the front and reslice; this
	// keeps the backing array compact (future appends reuse capacity) and is
	// O(maxHistory) only on overflow.
	if len(e.history) > maxHistory {
		copy(e.history, e.history[len(e.history)-maxHistory:])
		e.history = e.history[:maxHistory]
	}
	handlers := make([]EventHandler, len(e.handlers))
	copy(handlers, e.handlers)
	mode := e.mode
	writer := e.writer
	e.mu.Unlock()

	// Invoke handlers outside lock
	for _, h := range handlers {
		h(event)
	}

	// Write to stderr if enabled
	if (mode == OutputStderr || mode == OutputBoth) && writer != nil {
		data, err := json.Marshal(event)
		if err == nil {
			fmt.Fprintf(writer, "UI_EVENT: %s\n", string(data))
		}
	}

	// Fan out to every SSE subscriber (per-client channels).
	if mode == OutputSSE || mode == OutputBoth {
		e.mu.Lock()
		for _, ch := range e.subscribers {
			select {
			case ch <- event:
			default:
				// subscriber channel full, drop event (non-blocking)
			}
		}
		e.mu.Unlock()
	}
}

// EventOpt is a functional option for configuring a UIEvent.
type EventOpt func(*core.UIEvent)

func WithStatus(status string) EventOpt {
	return func(e *core.UIEvent) { e.Status = status }
}

func WithAgent(agent string) EventOpt {
	return func(e *core.UIEvent) { e.Agent = agent }
}

func WithGoal(goal string) EventOpt {
	return func(e *core.UIEvent) { e.Goal = goal }
}

func WithTool(tool string) EventOpt {
	return func(e *core.UIEvent) { e.Tool = tool }
}

func WithTaskID(id string) EventOpt {
	return func(e *core.UIEvent) { e.TaskID = id }
}

// Convenience methods

func (e *EventEmitter) EmitTaskUpdate(taskID, status, content string) {
	e.Emit(core.EventTaskUpdate, content,
		WithTaskID(taskID), WithStatus(status))
}

func (e *EventEmitter) EmitPlanUpdated(taskID, content string) {
	e.Emit(core.EventPlanUpdated, content, WithTaskID(taskID), WithStatus("plan_updated"))
}

func (e *EventEmitter) EmitWorkflowUpdated(taskID, content string) {
	e.Emit(core.EventWorkflowUpdated, content, WithTaskID(taskID), WithStatus("workflow_updated"))
}

func (e *EventEmitter) EmitAgentSpawn(role core.AgentRole, goal string) {
	e.Emit(core.EventAgentSpawn, goal,
		WithAgent(string(role)), WithGoal(goal), WithStatus("spawned"))
}

func (e *EventEmitter) EmitAgentComplete(role core.AgentRole, goal, output string, success bool) {
	status := "completed"
	if !success {
		status = "failed"
	}
	e.Emit(core.EventAgentComplete, output,
		WithAgent(string(role)), WithGoal(goal), WithStatus(status))
}

func (e *EventEmitter) EmitToolExecution(tool, status string, result interface{}) {
	e.Emit(core.EventToolExecution, result,
		WithTool(tool), WithStatus(status))
}

func (e *EventEmitter) EmitModelRoute(tier core.ModelTier, mode core.RoutingMode, detail string) {
	e.Emit(core.EventModelRoute, detail,
		WithStatus(string(mode)), WithAgent(string(tier)))
}

func (e *EventEmitter) EmitCompression(originalTokens, compressedTokens int) {
	e.Emit(core.EventCompression, fmt.Sprintf("%d -> %d tokens", originalTokens, compressedTokens),
		WithStatus("compressed"))
}

func (e *EventEmitter) EmitMemoryStore(memType core.MemoryType, key string) {
	e.Emit(core.EventMemoryStore, key,
		WithStatus(string(memType)))
}

func (e *EventEmitter) EmitFinalOutput(content string) {
	e.Emit(core.EventFinalOutput, content, WithStatus("final"))
}

func (e *EventEmitter) EmitChatQuery(content string) {
	e.Emit(core.EventChatQuery, content, WithStatus("user"))
}

func (e *EventEmitter) EmitChatResponse(content string) {
	e.Emit(core.EventChatResponse, content, WithStatus("assistant"))
}

func (e *EventEmitter) EmitSyncGUI() {
	e.Emit(core.EventSyncGUI, "sync", WithStatus("system"))
}

func (e *EventEmitter) EmitError(err string) {
	e.Emit(core.EventError, err, WithStatus("error"))
}

func (e *EventEmitter) EmitDAGUpdate(summary string) {
	e.Emit(core.EventDAGUpdate, summary, WithStatus("updated"))
}

func (e *EventEmitter) EmitSkillExtract(skillName string) {
	e.Emit(core.EventSkillExtract, skillName, WithStatus("extracted"))
}

func (e *EventEmitter) EmitConsensus(detail interface{}, conflict bool) {
	status := "consensus"
	if conflict {
		status = "conflict"
	}
	e.Emit(core.EventConsensus, detail, WithStatus(status))
}

// EmitTokenUsage pushes live token/cost telemetry to the monitoring dashboard.
func (e *EventEmitter) EmitTokenUsage(stats core.TokenUsageStats) {
	e.Emit(core.EventTokenUsage, stats, WithStatus("usage"))
}

// History returns all emitted events.
func (e *EventEmitter) History() []core.UIEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]core.UIEvent, len(e.history))
	copy(result, e.history)
	return result
}

// ClearHistory clears the event history.
func (e *EventEmitter) ClearHistory() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = e.history[:0]
}

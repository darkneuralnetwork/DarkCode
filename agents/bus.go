package agents

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkcode/core"
)

// ============================================================================
// AGENT COMMUNICATION BUS
//
// Structured message passing between agents using pub/sub pattern.
// Messages are typed (TaskAssignment, StatusUpdate, ResultReport, etc.)
// and include priority, correlation IDs, and sender/receiver metadata.
// ============================================================================

var msgCounter int64

func nextMsgID() string {
	return fmt.Sprintf("msg_%d", atomic.AddInt64(&msgCounter, 1))
}

// AgentBus provides structured inter-agent communication.
type AgentBus struct {
	mu          sync.RWMutex
	subscribers map[core.AgentRole][]chan core.AgentMessage
	broadcast   chan core.AgentMessage
	history     []core.AgentMessage
	maxHistory  int
}

// NewAgentBus creates a new agent communication bus.
func NewAgentBus() *AgentBus {
	return &AgentBus{
		subscribers: make(map[core.AgentRole][]chan core.AgentMessage),
		broadcast:   make(chan core.AgentMessage, 256),
		maxHistory:  500,
	}
}

// Send delivers a message to a specific agent role.
func (b *AgentBus) Send(msg core.AgentMessage) {
	b.mu.Lock()
	if msg.ID == "" {
		msg.ID = nextMsgID()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	b.history = append(b.history, msg)
	if len(b.history) > b.maxHistory {
		b.history = b.history[len(b.history)-b.maxHistory:]
	}

	subs := b.subscribers[msg.Receiver]
	b.mu.Unlock()

	// Deliver to all subscribers for this role
	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
			// Channel full, drop (non-blocking)
		}
	}

	// Also broadcast for monitoring
	select {
	case b.broadcast <- msg:
	default:
	}
}

// SendTask is a convenience method for task assignment messages.
func (b *AgentBus) SendTask(from, to core.AgentRole, task, payload string, priority core.MessagePriority) {
	b.Send(core.AgentMessage{
		Kind:     core.MsgTaskAssignment,
		Sender:   from,
		Receiver: to,
		Priority: priority,
		Task:     task,
		Payload:  payload,
	})
}

// SendResult is a convenience method for result report messages.
func (b *AgentBus) SendResult(from, to core.AgentRole, task, result, correlationID string) {
	b.Send(core.AgentMessage{
		Kind:          core.MsgResultReport,
		Sender:        from,
		Receiver:      to,
		Priority:      core.MsgPriorityNormal,
		Task:          task,
		Payload:       result,
		CorrelationID: correlationID,
	})
}

// Subscribe creates a channel that receives messages for a specific role.
func (b *AgentBus) Subscribe(role core.AgentRole) <-chan core.AgentMessage {
	ch := make(chan core.AgentMessage, 32)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[role] = append(b.subscribers[role], ch)
	return ch
}

// Unsubscribe removes a channel from a role's subscribers.
func (b *AgentBus) Unsubscribe(role core.AgentRole, ch <-chan core.AgentMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[role]
	for i, sub := range subs {
		if sub == ch {
			b.subscribers[role] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
}

// Broadcast returns the broadcast channel for monitoring all messages.
func (b *AgentBus) Broadcast() <-chan core.AgentMessage {
	return b.broadcast
}

// History returns recent messages, most recent first.
func (b *AgentBus) History() []core.AgentMessage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]core.AgentMessage, len(b.history))
	copy(result, b.history)
	// Reverse
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// RecentHistory returns the N most recent messages.
func (b *AgentBus) RecentHistory(n int) []core.AgentMessage {
	all := b.History()
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}

// MessageCount returns the total messages sent.
func (b *AgentBus) MessageCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.history)
}

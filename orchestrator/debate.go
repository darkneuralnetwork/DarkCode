package orchestrator

// debate.go — the kernel's side of adjudication: the runtime toggle and the
// bus adapter. The decision itself lives in package adjudicate; see its doc
// comment for why debate is a fallback and why it is capped at one round.

import (
	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
)

// SetDebate turns conflict-triggered debate on or off at runtime.
func (k *Kernel) SetDebate(on bool) {
	k.mu.Lock()
	k.debateOn = on
	k.mu.Unlock()
}

// debateEnabled reports whether an exchange may run.
func (k *Kernel) debateEnabled() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.debateOn
}

// critiqueBus records each side of an exchange on the agent bus, so the
// exchange is inspectable rather than invisible. The bus has carried
// MsgCritiqueRequest since it was written; this is its only traffic.
type critiqueBus struct{ k *Kernel }

func (b critiqueBus) Critiqued(from, to, goal, body string) {
	if b.k.agentBus == nil || body == "" {
		return
	}
	b.k.agentBus.Send(core.AgentMessage{
		Kind:     core.MsgCritiqueRequest,
		Sender:   core.RoleCritic,
		Receiver: core.RoleCritic,
		Priority: core.MsgPriorityNormal,
		Task:     strutil.Truncate(goal, 120),
		Payload:  from + " → " + to + ": " + body,
	})
}

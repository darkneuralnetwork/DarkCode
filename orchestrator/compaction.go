package orchestrator

// compaction.go — when to spend a model call compacting the conversation.
//
// WHY THIS CHANGED
//
// Compaction itself is not the problem and is not going away. Every production
// coding agent has it, because retrieval and compaction solve different things:
// retrieval fetches durable knowledge that exists somewhere else (the repo, the
// graph, past sessions), while compaction manages the agent's own working
// memory for the task in hand, which has no external source to fetch from.
//
// What was wrong was when it fired. Two triggers, either sufficient:
//
//	len(stm) >= 8 && grown by >= 4     — a message COUNT, unrelated to tokens
//	tokens > 60% of the window         — a watermark, far too low
//
// The count trigger is the worse of the two. Eight short turns can be five
// hundred tokens; it would spend an LLM call to summarise a conversation using
// a fraction of the window, then do it again four messages later. It measured
// the wrong thing entirely — there is no amount of message count that tells you
// whether you are near the context limit.
//
// The 60% watermark threw away information while 40% of the window sat unused.
// Published practice across shipping harnesses is 50–90%, usually expressed as
// "the window minus enough headroom to finish the current turn".
//
// So there is now one trigger and it is about tokens: compact when the
// conversation no longer leaves room to work.
//
// NOTE ON WHAT THIS DOES NOT FIX
//
// Compaction is still destructive — STMCompress replaces the buffer with a
// briefing plus a short tail, and the originals are gone. That is the next
// thing to fix, and it is tracked separately, because "when" and "what happens"
// are independent decisions and mixing them would make both hard to verify.

// compactionReserveTokens is the headroom kept free for the turn about to run:
// the user's message, the retrieved context, the tool schemas, and the model's
// reply. Compaction that leaves no room to answer has not helped.
//
// 16k is the low end of the 16–20k range harnesses converge on. Erring small
// means compacting slightly later, which is the cheaper direction to be wrong
// in — a turn that fits without compacting has cost nothing.
const compactionReserveTokens = 16000

// compactionMaxPercent caps how full the window may get regardless of reserve.
// On a very large window, reserve alone would wait until 92%+, which leaves the
// model reasoning over a window so full that quality degrades before the limit
// is reached.
const compactionMaxPercent = 85

// compactionMinPercent floors the trigger. On a small local window the reserve
// exceeds the window itself, and without this the threshold would go negative
// and compact on every single turn — the original bug in a new form.
const compactionMinPercent = 50

// compactionThreshold returns the token count at which the conversation should
// be compacted for a model with the given context window.
//
// Whichever of "window minus reserve" and "max percent" comes first wins, and
// the result never falls below compactionMinPercent of the window:
//
//	200,000 window → min(184,000, 170,000) = 170,000  (85%)
//	 32,000 window → min( 16,000,  27,200) =  16,000  (50%)
//	  8,000 window → min( -8,000,   6,800) → floored  =   4,000  (50%)
//
// Returns 0 when the window is unknown, which callers must treat as "do not
// compact" rather than "compact now" — guessing a window and acting on the
// guess is what a message count was doing.
func compactionThreshold(window int) int {
	if window <= 0 {
		return 0
	}
	threshold := window * compactionMaxPercent / 100
	if byReserve := window - compactionReserveTokens; byReserve < threshold {
		threshold = byReserve
	}
	if floor := window * compactionMinPercent / 100; threshold < floor {
		threshold = floor
	}
	return threshold
}

// shouldCompact reports whether a conversation of usedTokens should be
// compacted before the next turn against a model with the given window.
//
// A conversation of fewer than two messages is never compacted: there is
// nothing to summarise but the question just asked.
func shouldCompact(messageCount, usedTokens, window int) bool {
	if messageCount < 2 {
		return false
	}
	threshold := compactionThreshold(window)
	return threshold > 0 && usedTokens >= threshold
}

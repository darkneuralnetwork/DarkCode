package memory

// smalltalk.go — answering "hi" without asking a language model.
//
// The answer cache was built to stop trivial repeats from costing an LLM call,
// and the example always given for it is a greeting. It never worked on one:
// ConfidentRecall needs at least 4 content tokens and BestRecallAnswer needs 2,
// so "hi", "hello" and "thanks" fell through every rung and reached the model
// every single time. The cheapest possible request was the one paying full
// price, which matters most on a metered free tier where the daily request
// budget is measured in tens.
//
// A greeting does not need memory, retrieval or similarity scoring; it needs a
// reply. This answers it directly, before the cascade, at zero cost and zero
// latency.
//
// The bar for matching is deliberately high: the ENTIRE message must be
// smalltalk. "hi, can you fix the build" is a build request wearing a greeting,
// and answering it with "Hello" would be much worse than spending a call on it.

import "strings"

// smalltalkPhrases are complete messages that carry no task. Matched against
// the whole normalised message, never as a substring.
var smalltalkPhrases = map[string]string{
	"hi":                greetReply,
	"hii":               greetReply,
	"hey":               greetReply,
	"hello":             greetReply,
	"heya":              greetReply,
	"yo":                greetReply,
	"hi there":          greetReply,
	"hey there":         greetReply,
	"hello there":       greetReply,
	"good morning":      greetReply,
	"good afternoon":    greetReply,
	"good evening":      greetReply,
	"morning":           greetReply,
	"greetings":         greetReply,
	"howdy":             greetReply,
	"sup":               greetReply,
	"whats up":          greetReply,
	"what's up":         greetReply,
	"how are you":       howReply,
	"how are you doing": howReply,
	"how's it going":    howReply,
	"hows it going":     howReply,
	"you there":         howReply,
	"are you there":     howReply,

	"thanks":            thanksReply,
	"thank you":         thanksReply,
	"thanks a lot":      thanksReply,
	"thank you so much": thanksReply,
	"thanks so much":    thanksReply,
	"ty":                thanksReply,
	"thx":               thanksReply,
	"cheers":            thanksReply,
	"appreciate it":     thanksReply,
	"nice":              ackReply,
	"cool":              ackReply,
	"great":             ackReply,
	"awesome":           ackReply,
	"perfect":           ackReply,
	"ok":                ackReply,
	"okay":              ackReply,
	"k":                 ackReply,
	"got it":            ackReply,
	"understood":        ackReply,
	"sounds good":       ackReply,
	"good job":          ackReply,
	"well done":         ackReply,
	"nice work":         ackReply,

	"bye":             byeReply,
	"goodbye":         byeReply,
	"see you":         byeReply,
	"see ya":          byeReply,
	"good night":      byeReply,
	"goodnight":       byeReply,
	"later":           byeReply,
	"catch you later": byeReply,
}

const (
	greetReply  = "Hey — what are we working on?"
	howReply    = "Running fine and ready to go. What do you need?"
	thanksReply = "Anytime."
	ackReply    = "👍"
	byeReply    = "See you — I'll keep everything where you left it."
)

// smalltalkMaxLen bounds what is even considered. A message long enough to
// carry a real request is not smalltalk no matter how it opens, and this stops
// the normalisation below from being run over a pasted stack trace.
const smalltalkMaxLen = 40

// SmalltalkReply returns a canned reply when msg is nothing but a greeting,
// thanks, acknowledgement or sign-off, and (\"\", false) otherwise.
func SmalltalkReply(msg string) (string, bool) {
	key := normalizeSmalltalk(msg)
	if key == "" {
		return "", false
	}
	reply, ok := smalltalkPhrases[key]
	return reply, ok
}

// IsSmalltalk reports whether msg is nothing but smalltalk.
func IsSmalltalk(msg string) bool {
	_, ok := SmalltalkReply(msg)
	return ok
}

// normalizeSmalltalk reduces a message to a lookup key: lowercase, stripped of
// trailing punctuation and of a leading/trailing address to the agent, with
// runs of whitespace collapsed. Returns "" when the message is too long to be
// smalltalk or normalises to nothing.
func normalizeSmalltalk(msg string) string {
	s := strings.ToLower(strings.TrimSpace(msg))
	if s == "" || len(s) > smalltalkMaxLen {
		return ""
	}
	// A message containing sentence-ending punctuation mid-string is doing
	// more than one thing; only the last piece could be smalltalk, and a
	// greeting bolted onto a request is a request.
	if strings.ContainsAny(strings.TrimRight(s, ".!?"), ".!?") {
		return ""
	}
	s = strings.Trim(s, " \t.!?,")
	// "hi darkcode" / "darkcode hi" are still just a greeting.
	for _, name := range []string{"darkcode", "dark code", "agent", "bot"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, name))
		s = strings.TrimSpace(strings.TrimSuffix(s, name))
	}
	s = strings.Trim(s, " \t,")
	return strings.Join(strings.Fields(s), " ")
}

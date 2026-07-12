package router



// TieredModelPool extends the existing tier map with purpose-based grouping.
type TieredModelPool struct {
	Reasoning []RegisteredModel // complex tasks, architecture
	Coding    []RegisteredModel // implementation, debugging
	Review    []RegisteredModel // critic, verifier, analyst roles
	Fast      []RegisteredModel // compression, summaries, classification
	Local     []RegisteredModel // embedded/Ollama models
}

func NewTieredModelPool() *TieredModelPool {
	return &TieredModelPool{}
}

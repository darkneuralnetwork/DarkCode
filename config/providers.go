package config

// ============================================================================
// LLM PROVIDER REGISTRY
//
// A curated catalogue of LLM providers and their models, so models can be
// added to the system directly from the UI without memorising base URLs,
// auth schemes, or model identifiers. Pricing (USD per 1M tokens) is used by
// the metrics layer to estimate cost in the monitoring dashboard.
// ============================================================================

// ProviderModel describes a single model offered by a provider.
type ProviderModel struct {
	ID            string  `json:"id"`             // canonical model id sent to the API
	Name          string  `json:"name"`           // human-friendly label
	ContextWindow int     `json:"context_window"` // max context in tokens
	InputPrice    float64 `json:"input_price"`    // USD per 1M input tokens
	OutputPrice   float64 `json:"output_price"`   // USD per 1M output tokens
	// CachedInputPrice is the USD per 1M price for prompt tokens served from
	// the provider's prefix cache (much cheaper than InputPrice). 0 = not
	// specified; the cost calculator then falls back to a conservative 50% of
	// InputPrice (OpenAI's automatic-cache discount), so caching never
	// over-credits savings.
	CachedInputPrice float64 `json:"cached_input_price,omitempty"`
	Tier             string  `json:"tier"` // reasoning | coding | fast | vision
	Description      string  `json:"description,omitempty"`
}

// Provider describes an LLM provider endpoint.
type Provider struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	BaseURL       string            `json:"base_url"`    // OpenAI-compatible base (client appends /chat/completions)
	AuthScheme    string            `json:"auth_scheme"` // "bearer" | "api-key" | "none"
	ExtraHeaders  map[string]string `json:"extra_headers,omitempty"`
	ExtraQuery    string            `json:"extra_query,omitempty"` // e.g. api-version=... for Azure
	Local         bool              `json:"local"`                 // true for self-hosted (Ollama, LM Studio)
	CustomBaseURL bool              `json:"custom_base_url"`       // true for OpenAI-compatible custom endpoints: honour caller base_url + allow loopback/private SSRF targets, but display as a normal (non-local) provider
	DocsURL       string            `json:"docs_url,omitempty"`
	KeyURL        string            `json:"key_url,omitempty"` // where to obtain an API key
	Models        []ProviderModel   `json:"models"`
}

// Auth schemes
const (
	AuthBearer = "bearer"
	AuthAPIKey = "api-key"
	AuthNone   = "none"
)

// providers is the canonical registry. Order is preserved for the UI.
var providers = []Provider{
	// ---------------------------------------------------------------- OpenAI
	{
		ID:         "openai",
		Name:       "OpenAI",
		BaseURL:    "https://api.openai.com/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://platform.openai.com/docs",
		KeyURL:     "https://platform.openai.com/api-keys",
		Models: []ProviderModel{
			{ID: "gpt-4o", Name: "GPT-4o", ContextWindow: 128000, InputPrice: 2.50, OutputPrice: 10.00, Tier: "reasoning", Description: "Flagship multimodal model"},
			{ID: "gpt-4o-mini", Name: "GPT-4o mini", ContextWindow: 128000, InputPrice: 0.15, OutputPrice: 0.60, Tier: "fast", Description: "Affordable small model"},
			{ID: "gpt-4.1", Name: "GPT-4.1", ContextWindow: 1047576, InputPrice: 2.00, OutputPrice: 8.00, Tier: "reasoning", Description: "Long-context coding & reasoning"},
			{ID: "gpt-4.1-mini", Name: "GPT-4.1 mini", ContextWindow: 1047576, InputPrice: 0.40, OutputPrice: 1.60, Tier: "coding", Description: "Balanced long-context"},
			{ID: "gpt-4.1-nano", Name: "GPT-4.1 nano", ContextWindow: 1047576, InputPrice: 0.10, OutputPrice: 0.40, Tier: "fast", Description: "Lowest-cost model"},
			{ID: "o3", Name: "o3", ContextWindow: 200000, InputPrice: 10.00, OutputPrice: 40.00, Tier: "reasoning", Description: "Advanced reasoning"},
			{ID: "o3-mini", Name: "o3-mini", ContextWindow: 200000, InputPrice: 1.10, OutputPrice: 4.40, Tier: "reasoning", Description: "Fast reasoning"},
			{ID: "o4-mini", Name: "o4-mini", ContextWindow: 200000, InputPrice: 1.10, OutputPrice: 4.40, Tier: "reasoning", Description: "Latest efficient reasoner"},
		},
	},
	// ------------------------------------------------------------ Anthropic
	{
		ID:         "anthropic",
		Name:       "Anthropic (Claude)",
		BaseURL:    "https://api.anthropic.com/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.anthropic.com",
		KeyURL:     "https://console.anthropic.com/settings/keys",
		Models: []ProviderModel{
			{ID: "claude-3-5-sonnet-20241022", Name: "Claude 3.5 Sonnet", ContextWindow: 200000, InputPrice: 3.00, OutputPrice: 15.00, Tier: "reasoning", Description: "Top coding & reasoning"},
			{ID: "claude-3-5-haiku-20241022", Name: "Claude 3.5 Haiku", ContextWindow: 200000, InputPrice: 0.80, OutputPrice: 4.00, Tier: "fast", Description: "Fast & affordable"},
			{ID: "claude-3-opus-20240229", Name: "Claude 3 Opus", ContextWindow: 200000, InputPrice: 15.00, OutputPrice: 75.00, Tier: "reasoning", Description: "Deep reasoning"},
			{ID: "claude-3-sonnet-20240229", Name: "Claude 3 Sonnet", ContextWindow: 200000, InputPrice: 3.00, OutputPrice: 15.00, Tier: "coding", Description: "Balanced"},
			{ID: "claude-3-haiku-20240307", Name: "Claude 3 Haiku", ContextWindow: 200000, InputPrice: 0.25, OutputPrice: 1.25, Tier: "fast", Description: "Lightweight"},
		},
	},
	// ----------------------------------------------- Google (Gemini, OpenAI-compat)
	{
		ID:         "google",
		Name:       "Google (Gemini)",
		BaseURL:    "https://generativelanguage.googleapis.com/v1beta/openai",
		AuthScheme: AuthBearer,
		DocsURL:    "https://ai.google.dev/docs",
		KeyURL:     "https://aistudio.google.com/app/apikey",
		Models: []ProviderModel{
			{ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash", ContextWindow: 1048576, InputPrice: 0.15, OutputPrice: 0.60, Tier: "fast", Description: "Latest fast model"},
			{ID: "gemini-2.5-pro", Name: "Gemini 2.5 Pro", ContextWindow: 1048576, InputPrice: 1.25, OutputPrice: 10.00, Tier: "reasoning", Description: "Latest reasoning flagship"},
			{ID: "gemini-2.0-flash", Name: "Gemini 2.0 Flash", ContextWindow: 1048576, InputPrice: 0.10, OutputPrice: 0.40, Tier: "fast", Description: "Fast 1M-context model"},
			{ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro (Preview)", ContextWindow: 1048576, InputPrice: 2.50, OutputPrice: 15.00, Tier: "reasoning", Description: "Preview of next-gen pro"},
			{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash", ContextWindow: 1048576, InputPrice: 0.15, OutputPrice: 0.60, Tier: "fast", Description: "Next-gen fast model"},
		},
	},
	// ------------------------------------------------------------ DeepSeek
	{
		ID:         "deepseek",
		Name:       "DeepSeek",
		BaseURL:    "https://api.deepseek.com",
		AuthScheme: AuthBearer,
		DocsURL:    "https://api-docs.deepseek.com",
		KeyURL:     "https://platform.deepseek.com/api_keys",
		Models: []ProviderModel{
			{ID: "deepseek-chat", Name: "DeepSeek-V3 (Chat)", ContextWindow: 65536, InputPrice: 0.27, OutputPrice: 1.10, Tier: "coding", Description: "General V3 model"},
			{ID: "deepseek-reasoner", Name: "DeepSeek-R1 (Reasoner)", ContextWindow: 65536, InputPrice: 0.55, OutputPrice: 2.19, Tier: "reasoning", Description: "R1 reasoning model"},
			{ID: "deepseek-coder", Name: "DeepSeek Coder", ContextWindow: 65536, InputPrice: 0.14, OutputPrice: 0.28, Tier: "coding", Description: "Code-specialised"},
		},
	},
	// --------------------------------------------------------- OpenRouter
	{
		ID:         "openrouter",
		Name:       "OpenRouter",
		BaseURL:    "https://openrouter.ai/api/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://openrouter.ai/docs",
		KeyURL:     "https://openrouter.ai/keys",
		Models: []ProviderModel{
			{ID: "anthropic/claude-3.5-sonnet", Name: "Claude 3.5 Sonnet", ContextWindow: 200000, InputPrice: 3.00, OutputPrice: 15.00, Tier: "reasoning", Description: "Via OpenRouter"},
			{ID: "openai/gpt-4o", Name: "GPT-4o", ContextWindow: 128000, InputPrice: 2.50, OutputPrice: 10.00, Tier: "reasoning", Description: "Via OpenRouter"},
			{ID: "google/gemini-2.0-flash-001", Name: "Gemini 2.0 Flash", ContextWindow: 1048576, InputPrice: 0.10, OutputPrice: 0.40, Tier: "fast", Description: "Via OpenRouter"},
			{ID: "meta-llama/llama-3.3-70b-instruct", Name: "Llama 3.3 70B", ContextWindow: 128000, InputPrice: 0.23, OutputPrice: 0.40, Tier: "coding", Description: "Open weights"},
			{ID: "deepseek/deepseek-r1", Name: "DeepSeek R1", ContextWindow: 65536, InputPrice: 0.55, OutputPrice: 2.19, Tier: "reasoning", Description: "Via OpenRouter"},
			{ID: "qwen/qwen-2.5-72b-instruct", Name: "Qwen 2.5 72B", ContextWindow: 131072, InputPrice: 0.23, OutputPrice: 0.40, Tier: "coding", Description: "Via OpenRouter"},
		},
	},
	// ---------------------------------------------------------------- Groq
	{
		ID:         "groq",
		Name:       "Groq",
		BaseURL:    "https://api.groq.com/openai/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://console.groq.com/docs",
		KeyURL:     "https://console.groq.com/keys",
		Models: []ProviderModel{
			{ID: "llama-3.3-70b-versatile", Name: "Llama 3.3 70B", ContextWindow: 128000, InputPrice: 0.59, OutputPrice: 0.79, Tier: "coding", Description: "Ultra-fast inference"},
			{ID: "llama-3.1-8b-instant", Name: "Llama 3.1 8B", ContextWindow: 128000, InputPrice: 0.05, OutputPrice: 0.08, Tier: "fast", Description: "Fastest small model"},
			{ID: "mixtral-8x7b-32768", Name: "Mixtral 8x7B", ContextWindow: 32768, InputPrice: 0.24, OutputPrice: 0.24, Tier: "coding", Description: "MoE model"},
			{ID: "gemma2-9b-it", Name: "Gemma 2 9B", ContextWindow: 8192, InputPrice: 0.20, OutputPrice: 0.20, Tier: "fast", Description: "Google open model"},
		},
	},
	// ------------------------------------------------------------- Mistral
	{
		ID:         "mistral",
		Name:       "Mistral AI",
		BaseURL:    "https://api.mistral.ai/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.mistral.ai",
		KeyURL:     "https://console.mistral.ai/api-keys",
		Models: []ProviderModel{
			{ID: "mistral-large-latest", Name: "Mistral Large", ContextWindow: 128000, InputPrice: 2.00, OutputPrice: 6.00, Tier: "reasoning", Description: "Flagship"},
			{ID: "mistral-small-latest", Name: "Mistral Small", ContextWindow: 32000, InputPrice: 0.20, OutputPrice: 0.60, Tier: "fast", Description: "Efficient"},
			{ID: "codestral-latest", Name: "Codestral", ContextWindow: 256000, InputPrice: 0.30, OutputPrice: 0.90, Tier: "coding", Description: "Code-specialised"},
			{ID: "open-mistral-nemo", Name: "Mistral Nemo", ContextWindow: 128000, InputPrice: 0.15, OutputPrice: 0.15, Tier: "coding", Description: "Open weights 12B"},
		},
	},
	// ----------------------------------------------------------------- xAI
	{
		ID:         "xai",
		Name:       "xAI (Grok)",
		BaseURL:    "https://api.x.ai/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.x.ai",
		KeyURL:     "https://console.x.ai",
		Models: []ProviderModel{
			{ID: "grok-2-latest", Name: "Grok 2", ContextWindow: 131072, InputPrice: 2.00, OutputPrice: 10.00, Tier: "reasoning", Description: "Latest Grok"},
			{ID: "grok-2-vision-latest", Name: "Grok 2 Vision", ContextWindow: 32768, InputPrice: 2.00, OutputPrice: 10.00, Tier: "vision", Description: "Multimodal Grok"},
			{ID: "grok-beta", Name: "Grok Beta", ContextWindow: 131072, InputPrice: 5.00, OutputPrice: 15.00, Tier: "reasoning", Description: "Early Grok"},
		},
	},
	// --------------------------------------------------------- Together AI
	{
		ID:         "together",
		Name:       "Together AI",
		BaseURL:    "https://api.together.xyz/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.together.ai",
		KeyURL:     "https://api.together.ai/settings/api-keys",
		Models: []ProviderModel{
			{ID: "meta-llama/Llama-3.3-70B-Instruct-Turbo", Name: "Llama 3.3 70B Turbo", ContextWindow: 128000, InputPrice: 0.88, OutputPrice: 0.88, Tier: "coding", Description: "Optimised Llama"},
			{ID: "meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo", Name: "Llama 3.1 8B Turbo", ContextWindow: 128000, InputPrice: 0.18, OutputPrice: 0.18, Tier: "fast", Description: "Small & fast"},
			{ID: "Qwen/Qwen2.5-72B-Instruct-Turbo", Name: "Qwen 2.5 72B", ContextWindow: 32768, InputPrice: 1.20, OutputPrice: 1.20, Tier: "coding", Description: "Strong coding model"},
			{ID: "deepseek-ai/DeepSeek-R1", Name: "DeepSeek R1", ContextWindow: 128000, InputPrice: 1.08, OutputPrice: 1.08, Tier: "reasoning", Description: "Hosted R1"},
		},
	},
	// --------------------------------------------------------- Fireworks AI
	{
		ID:         "fireworks",
		Name:       "Fireworks AI",
		BaseURL:    "https://api.fireworks.ai/inference/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.fireworks.ai",
		KeyURL:     "https://fireworks.ai/account/api-keys",
		Models: []ProviderModel{
			{ID: "accounts/fireworks/models/llama-v3p1-405b-instruct", Name: "Llama 3.1 405B", ContextWindow: 131072, InputPrice: 3.00, OutputPrice: 3.00, Tier: "reasoning", Description: "Largest open model"},
			{ID: "accounts/fireworks/models/llama-v3p1-70b-instruct", Name: "Llama 3.1 70B", ContextWindow: 131072, InputPrice: 0.90, OutputPrice: 0.90, Tier: "coding", Description: "Balanced"},
			{ID: "accounts/fireworks/models/llama-v3p1-8b-instruct", Name: "Llama 3.1 8B", ContextWindow: 131072, InputPrice: 0.20, OutputPrice: 0.20, Tier: "fast", Description: "Small"},
		},
	},
	// --------------------------------------------------------- Perplexity
	{
		ID:         "perplexity",
		Name:       "Perplexity",
		BaseURL:    "https://api.perplexity.ai",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.perplexity.ai",
		KeyURL:     "https://www.perplexity.ai/settings/api",
		Models: []ProviderModel{
			{ID: "llama-3.1-sonar-large-128k-online", Name: "Sonar Large Online", ContextWindow: 127072, InputPrice: 1.00, OutputPrice: 1.00, Tier: "reasoning", Description: "Web-grounded"},
			{ID: "llama-3.1-sonar-small-128k-online", Name: "Sonar Small Online", ContextWindow: 127072, InputPrice: 0.20, OutputPrice: 0.20, Tier: "fast", Description: "Web-grounded"},
			{ID: "llama-3.1-sonar-huge-128k-online", Name: "Sonar Huge Online", ContextWindow: 127072, InputPrice: 5.00, OutputPrice: 5.00, Tier: "reasoning", Description: "Largest Sonar"},
		},
	},
	// ------------------------------------------------------------ Cohere
	{
		ID:         "cohere",
		Name:       "Cohere",
		BaseURL:    "https://api.cohere.ai/compatibility/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.cohere.com",
		KeyURL:     "https://dashboard.cohere.com/api-keys",
		Models: []ProviderModel{
			{ID: "command-r-plus-08-2024", Name: "Command R+", ContextWindow: 128000, InputPrice: 2.50, OutputPrice: 10.00, Tier: "reasoning", Description: "Enterprise model"},
			{ID: "command-r-08-2024", Name: "Command R", ContextWindow: 128000, InputPrice: 0.15, OutputPrice: 0.60, Tier: "coding", Description: "Scalable model"},
			{ID: "command-r7b-12-2024", Name: "Command R7B", ContextWindow: 128000, InputPrice: 0.0375, OutputPrice: 0.15, Tier: "fast", Description: "Compact"},
		},
	},
	// ------------------------------------------------------------- NVIDIA
	{
		ID:         "nvidia",
		Name:       "NVIDIA NIM",
		BaseURL:    "https://integrate.api.nvidia.com/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.api.nvidia.com",
		KeyURL:     "https://build.nvidia.com",
		Models: []ProviderModel{
			{ID: "nvidia/llama-3.1-nemotron-70b-instruct", Name: "Nemotron 70B", ContextWindow: 131072, InputPrice: 0.12, OutputPrice: 0.12, Tier: "reasoning", Description: "NVIDIA-tuned Llama"},
			{ID: "meta/llama-3.1-405b-instruct", Name: "Llama 3.1 405B", ContextWindow: 131072, InputPrice: 0.53, OutputPrice: 0.53, Tier: "reasoning", Description: "Hosted 405B"},
			{ID: "meta/llama-3.1-70b-instruct", Name: "Llama 3.1 70B", ContextWindow: 131072, InputPrice: 0.13, OutputPrice: 0.13, Tier: "coding", Description: "Hosted 70B"},
		},
	},
	// ---------------------------------------------------------- Cloudflare
	{
		ID:         "cloudflare",
		Name:       "Cloudflare Workers AI",
		BaseURL:    "https://api.cloudflare.com/client/v4/accounts/{ACCOUNT_ID}/ai/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://developers.cloudflare.com/workers-ai",
		KeyURL:     "https://dash.cloudflare.com/profile/api-tokens",
		Models: []ProviderModel{
			{ID: "@cf/meta/llama-3.1-8b-instruct", Name: "Llama 3.1 8B", ContextWindow: 8192, InputPrice: 0, OutputPrice: 0, Tier: "fast", Description: "Edge inference"},
			{ID: "@cf/meta/llama-3.1-70b-instruct", Name: "Llama 3.1 70B", ContextWindow: 8192, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Edge inference"},
			{ID: "@cf/qwen/qwen1.5-14b-chat-awq", Name: "Qwen 1.5 14B", ContextWindow: 8192, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Edge inference"},
		},
	},
	// ---------------------------------------------------------- Hyperbolic
	{
		ID:         "hyperbolic",
		Name:       "Hyperbolic",
		BaseURL:    "https://api.hyperbolic.xyz/v1",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.hyperbolic.xyz",
		KeyURL:     "https://app.hyperbolic.xyz/settings",
		Models: []ProviderModel{
			{ID: "meta-llama/Meta-Llama-3.1-70B-Instruct", Name: "Llama 3.1 70B", ContextWindow: 131072, InputPrice: 0.40, OutputPrice: 0.40, Tier: "coding", Description: "GPU marketplace"},
			{ID: "meta-llama/Meta-Llama-3.1-405B-Instruct", Name: "Llama 3.1 405B", ContextWindow: 131072, InputPrice: 0.80, OutputPrice: 0.80, Tier: "reasoning", Description: "Largest open model"},
			{ID: "Qwen/Qwen2.5-72B-Instruct", Name: "Qwen 2.5 72B", ContextWindow: 131072, InputPrice: 0.40, OutputPrice: 0.40, Tier: "coding", Description: "Strong coding"},
		},
	},
	// -------------------------------------------------------------- Novita
	{
		ID:         "novita",
		Name:       "Novita AI",
		BaseURL:    "https://api.novita.ai/v3/openai",
		AuthScheme: AuthBearer,
		DocsURL:    "https://docs.novita.ai",
		KeyURL:     "https://novita.ai/settings/key-management",
		Models: []ProviderModel{
			{ID: "meta-llama/llama-3.1-70b-instruct", Name: "Llama 3.1 70B", ContextWindow: 131072, InputPrice: 0.55, OutputPrice: 0.76, Tier: "coding", Description: "Cost-efficient"},
			{ID: "meta-llama/llama-3.1-8b-instruct", Name: "Llama 3.1 8B", ContextWindow: 131072, InputPrice: 0.05, OutputPrice: 0.05, Tier: "fast", Description: "Small & cheap"},
			{ID: "deepseek/deepseek-r1", Name: "DeepSeek R1", ContextWindow: 131072, InputPrice: 0.40, OutputPrice: 0.40, Tier: "reasoning", Description: "Hosted R1"},
		},
	},
	// --------------------------------------------------------------- Azure
	{
		ID:         "azure",
		Name:       "Azure OpenAI",
		BaseURL:    "https://{RESOURCE}.openai.azure.com/openai/deployments/{DEPLOYMENT}",
		AuthScheme: AuthAPIKey,
		ExtraQuery: "api-version=2024-10-21",
		DocsURL:    "https://learn.microsoft.com/azure/ai-services/openai",
		KeyURL:     "https://portal.azure.com",
		Models: []ProviderModel{
			{ID: "gpt-4o", Name: "GPT-4o (deployment)", ContextWindow: 128000, InputPrice: 2.50, OutputPrice: 10.00, Tier: "reasoning", Description: "Set base URL with your deployment"},
			{ID: "gpt-4o-mini", Name: "GPT-4o mini (deployment)", ContextWindow: 128000, InputPrice: 0.15, OutputPrice: 0.60, Tier: "fast", Description: "Set base URL with your deployment"},
			{ID: "gpt-4", Name: "GPT-4 (deployment)", ContextWindow: 8192, InputPrice: 30.00, OutputPrice: 60.00, Tier: "reasoning", Description: "Set base URL with your deployment"},
		},
	},
	// ---------------------------------------------------- llama.cpp (Embedded)
	{
		ID:         "embedded",
		Name:       "llama.cpp (Embedded)",
		BaseURL:    "http://127.0.0.1:0/v1", // Dynamically allocated port
		AuthScheme: AuthNone,
		Local:      true,
		DocsURL:    "https://github.com/ggerganov/llama.cpp",
		KeyURL:     "",
		Models: []ProviderModel{
			{ID: "local-gguf", Name: "Local GGUF Model", ContextWindow: 4096, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Local models from the configured models directory"},
		},
	},
	// -------------------------------------------------------------- Ollama
	{
		ID:         "ollama",
		Name:       "Ollama (Local)",
		BaseURL:    "http://localhost:11434/v1",
		AuthScheme: AuthNone,
		Local:      true,
		DocsURL:    "https://ollama.com",
		KeyURL:     "",
		Models: []ProviderModel{
			{ID: "llama3.1", Name: "Llama 3.1", ContextWindow: 131072, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Local — pull with: ollama pull llama3.1"},
			{ID: "qwen2.5", Name: "Qwen 2.5", ContextWindow: 131072, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Local coding model"},
			{ID: "deepseek-r1", Name: "DeepSeek R1", ContextWindow: 131072, InputPrice: 0, OutputPrice: 0, Tier: "reasoning", Description: "Local reasoning model"},
			{ID: "mistral", Name: "Mistral", ContextWindow: 32768, InputPrice: 0, OutputPrice: 0, Tier: "fast", Description: "Local Mistral"},
			{ID: "codellama", Name: "Code Llama", ContextWindow: 16384, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Local code model"},
			{ID: "gemma2", Name: "Gemma 2", ContextWindow: 8192, InputPrice: 0, OutputPrice: 0, Tier: "fast", Description: "Local Gemma"},
		},
	},
	// ----------------------------------------------------------- LM Studio
	{
		ID:         "lmstudio",
		Name:       "LM Studio (Local)",
		BaseURL:    "http://localhost:1234/v1",
		AuthScheme: AuthBearer,
		Local:      true,
		DocsURL:    "https://lmstudio.ai/docs",
		KeyURL:     "",
		Models: []ProviderModel{
			{ID: "local-model", Name: "Loaded Model", ContextWindow: 32768, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Any model loaded in LM Studio"},
		},
	},
	// ---------------------------------------------- OpenAI-Compatible (custom)
	// Any endpoint that speaks the OpenAI /v1/chat/completions protocol
	// (vLLM, LiteLLM, a proxy, etc.). The user supplies base URL + model id
	// + API key. CustomBaseURL=true so the SSRF-hardened fetch path honours
	// the caller-supplied base_url and allows loopback/private targets (a
	// local vLLM or a private proxy), while AuthBearer keeps the user's key
	// (it is NOT AuthNone, so the key is sent to the user's chosen endpoint,
	// never silently dropped). Local stays false so the UI shows the real
	// auth scheme (bearer) rather than a misleading "LOCAL" badge.
	{
		ID:            "openai-compatible",
		Name:          "OpenAI Compatible (Custom)",
		BaseURL:       "",
		AuthScheme:    AuthBearer,
		Local:         false,
		CustomBaseURL: true,
		DocsURL:       "https://platform.openai.com/docs/api-reference",
		KeyURL:        "",
		Models: []ProviderModel{
			{ID: "custom-model", Name: "Custom Model", ContextWindow: 0, InputPrice: 0, OutputPrice: 0, Tier: "coding", Description: "Any OpenAI-compatible model — enter its id and base URL below"},
		},
	},
}

// Providers returns the full provider registry.
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// LookupProvider returns a provider by id and whether it was found.
func LookupProvider(id string) (Provider, bool) {
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// LookupModel returns the provider model definition for a provider+model id.
func LookupModel(providerID, modelID string) (ProviderModel, bool) {
	p, ok := LookupProvider(providerID)
	if !ok {
		return ProviderModel{}, false
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m, true
		}
	}
	return ProviderModel{}, false
}

// LookupPricing returns the input/output price (USD per 1M tokens) for a
// provider+model. Returns (0,0,false) when unknown (cost is then untracked).
func LookupPricing(providerID, modelID string) (float64, float64, bool) {
	m, ok := LookupModel(providerID, modelID)
	if !ok {
		return 0, 0, false
	}
	return m.InputPrice, m.OutputPrice, true
}

// LookupPricingFull is LookupPricing plus the cached-input price used to
// discount prefix-cache hits. When a model has no explicit CachedInputPrice,
// cached tokens are priced at 50% of InputPrice — the conservative floor
// (OpenAI's automatic-cache rate; Anthropic's is cheaper, so this never
// over-credits the saving).
func LookupPricingFull(providerID, modelID string) (in, cachedIn, out float64, ok bool) {
	m, found := LookupModel(providerID, modelID)
	if !found {
		return 0, 0, 0, false
	}
	cachedIn = m.CachedInputPrice
	if cachedIn <= 0 {
		cachedIn = m.InputPrice * 0.5
	}
	return m.InputPrice, cachedIn, m.OutputPrice, true
}

// ResolveTier returns the tier for a provider+model from the registry, falling
// back to "coding" when the model is not in the catalogue. This keeps the
// tier assignment identical whether a model is added via CLI, GUI, or .config.
func ResolveTier(providerID, modelID string) string {
	if m, ok := LookupModel(providerID, modelID); ok && m.Tier != "" {
		return m.Tier
	}
	if p, ok := LookupProvider(providerID); ok && p.Local {
		return "local"
	}
	return "coding"
}

// ProviderIDs returns the list of provider ids (useful for presets).
func ProviderIDs() []string {
	out := make([]string, len(providers))
	for i, p := range providers {
		out[i] = p.ID
	}
	return out
}

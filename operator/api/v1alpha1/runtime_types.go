package v1alpha1

// RuntimeSpec selects the execution engine and the model route.
type RuntimeSpec struct {
	// Type of the execution engine. Only "deerflow" is implemented.
	// +kubebuilder:validation:Enum=deerflow
	Type string `json:"type"`

	// Image is the container image for the runtime (must embed the
	// governance middleware and MCP tooling).
	Image string `json:"image"`

	// Models lists the LLM routes the runtime may use, in priority order:
	// the first entry is the default the runtime picks when a caller does
	// not name one, later entries are selectable by name (DeerFlow resolves
	// per-request and per-subagent model choices against this list — it is
	// selection, not automatic failover). When empty the legacy singular
	// fields below define the single model.
	// +optional
	Models []RuntimeModel `json:"models,omitempty"`

	// Model is the wire model id routed by the Higress model gateway
	// (e.g. bench-glm). Never a provider-native model name. Legacy form of
	// Models[0]; kept for existing CRs.
	Model string `json:"model"`

	// ModelBaseURL is the model API base URL. Defaults to the in-cluster
	// Higress gateway when empty. Legacy form of Models[0].baseURL.
	// +optional
	ModelBaseURL string `json:"modelBaseURL,omitempty"`

	// Provider selects the model protocol family. "openai" (default) keeps
	// the Higress-routed OpenAI-compatible path; "anthropic" speaks the
	// Anthropic wire protocol — the AEP model registry serves it under a
	// per-model path prefix (http://<gateway>/<model-id>).
	// +optional
	// +kubebuilder:validation:Enum=openai;anthropic
	Provider string `json:"provider,omitempty"`

	// NativeModel is the provider-native model id behind the wire name
	// (Model stays the stable wire id agents and threads reference).
	// Defaults to Model (Higress passthrough). Legacy form of
	// Models[0].nativeModel.
	// +optional
	NativeModel string `json:"nativeModel,omitempty"`

	// ApiKeyEnv names the credentials-Secret env var holding the provider
	// API key. Defaults to DEER_BENCH_API_KEY. Legacy form of
	// Models[0].apiKeyEnv.
	// +optional
	ApiKeyEnv string `json:"apiKeyEnv,omitempty"`
}

// RuntimeModel is one LLM route in the runtime's model list.
type RuntimeModel struct {
	// Name is the wire model id (AEP model id) agents and threads
	// reference; also the selection key inside the runtime config.
	Name string `json:"name"`

	// Provider selects the protocol family: "openai" or "anthropic".
	// +kubebuilder:validation:Enum=openai;anthropic
	Provider string `json:"provider"`

	// NativeModel is the provider-native model id sent upstream. Defaults
	// to Name.
	// +optional
	NativeModel string `json:"nativeModel,omitempty"`

	// BaseURL is the model API base URL. For anthropic models served by
	// the AEP registry this is <gateway>/<model-id>.
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// ApiKeyEnv names the credentials-Secret env var holding the provider
	// API key.
	// +optional
	ApiKeyEnv string `json:"apiKeyEnv,omitempty"`

	// Reasoning declares the model's thinking behavior so the runtime does
	// not have to infer it (DeerFlow turns thinking off for undeclared
	// models even when the upstream emits reasoning_content). Derived from
	// the AEP catalog's reasoningCompatibility.
	// +optional
	Reasoning *RuntimeReasoning `json:"reasoning,omitempty"`
}

// RuntimeReasoning is the declarative reasoning contract for one model,
// mirroring DeerFlow's config-level reasoning block (thinking/history subset
// the AEP catalog can express today).
type RuntimeReasoning struct {
	// Thinking policy: unsupported (never), optional (caller toggles,
	// default on), required (always on). Mapped from a declared
	// thinkingFormat.
	// +kubebuilder:validation:Enum=unsupported;optional;required
	// +optional
	Thinking string `json:"thinking,omitempty"`

	// History policy for reasoning across turns: preserve keeps
	// reasoning_content on assistant messages (required by
	// requiresReasoningContentOnAssistantMessages upstreams), clear drops
	// it. Empty leaves the runtime default.
	// +kubebuilder:validation:Enum=preserve;clear
	// +optional
	History string `json:"history,omitempty"`
}

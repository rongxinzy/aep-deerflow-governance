// Package v1alpha1 defines the DigitalEmployee custom resource.
//
// A DigitalEmployee is the declarative form of one governed agent instance:
// the CR name is the business ID that memory and knowledge are bound to.
// The operator holds full CRUD on this type; who may create one is a
// business-layer policy question and never a Kubernetes RBAC question.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// MemorySpec configures long-term memory persistence.
type MemorySpec struct {
	// Type of memory backend. Only "openviking" is implemented.
	// +kubebuilder:validation:Enum=openviking
	Type string `json:"type"`

	// ServerURL of the OpenViking service.
	ServerURL string `json:"serverURL"`

	// AccountID is the OpenViking account the employee user lives in.
	AccountID string `json:"accountID"`

	// SingleTenantInstance maps every runtime user of this single-tenant
	// instance onto the employee's memory owner (per-request human identity
	// lives in governance, not in the memory tenant).
	// +optional
	// +kubebuilder:default=false
	SingleTenantInstance bool `json:"singleTenantInstance,omitempty"`

	// MemoryScope selects how OpenViking memory is partitioned inside this
	// employee. "instance" (default) keeps the shipped behavior: memory
	// belongs to the employee instance, so every requester shares one space.
	// "requester" partitions memory per requesting user (DeerFlow's
	// memory_scope knob), so several humans on one instance stop sharing one
	// memory space. Instance-to-instance isolation is unchanged either way.
	// +optional
	// +kubebuilder:validation:Enum=instance;requester
	// +kubebuilder:default=instance
	MemoryScope string `json:"memoryScope,omitempty"`

	// SharedPeerID is an optional curated shared memory bucket, read in
	// addition to the requester's own memory when MemoryScope is "requester".
	// It must use the backend-reserved "df-shared-" prefix. Empty means
	// personal memory only. Ignored when MemoryScope is "instance".
	// +optional
	SharedPeerID string `json:"sharedPeerID,omitempty"`

	// Retrieval tuning; zero values fall back to the backend defaults.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TopK int32 `json:"topK,omitempty"`
	// +optional

	// +kubebuilder:validation:Maximum=1
	ScoreThreshold float64 `json:"scoreThreshold,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=256
	// +kubebuilder:validation:Maximum=100000
	MaxInjectionChars int32 `json:"maxInjectionChars,omitempty"`
}

// KnowledgeSpec wires the enterprise knowledge base (WeKnora) via MCP.
type KnowledgeSpec struct {
	// Type of knowledge integration. Only "weknora" is implemented.
	// +kubebuilder:validation:Enum=weknora
	Type string `json:"type"`

	// MCPURL of the WeKnora MCP bridge endpoint.
	MCPURL string `json:"mcpURL"`

	// Bases is the employee's explicit knowledge-base whitelist (WeKnora
	// kb ids plus display-name snapshots). Non-nil means an explicit
	// policy: the operator renders it into the governance middleware,
	// which denies knowledge_search calls outside the list (an empty
	// non-nil list denies every search). Nil means no policy configured:
	// the middleware treats that as a configuration error and
	// default-denies every knowledge tool with administrator guidance,
	// never an unrestricted bridge credential.
	// +optional
	// +kubebuilder:validation:MaxItems=200
	// No omitempty: an explicitly empty list (deny every search) must
	// survive JSON round-trips; absent/null decodes back to nil (= no
	// policy, default-deny).
	Bases []KnowledgeBaseRef `json:"bases"`
}

// KnowledgeBaseRef references one WeKnora knowledge base by id with a
// display-name snapshot for the console.
type KnowledgeBaseRef struct {
	// ID is the WeKnora knowledge-base id.
	ID string `json:"id"`
	// Name is the knowledge-base display name at assignment time.
	Name string `json:"name"`
}

// SkillRef registers one AEP skill enabled for the employee, pinned to the
// published version chosen at creation.
type SkillRef struct {
	// ID is the AEP skill identifier (server-generated slug).
	ID string `json:"id"`
	// Name is the skill display name at assignment time.
	Name string `json:"name"`
	// Version is the published skill version to use.
	Version string `json:"version"`
}

// VisibilitySpec is the employee's explicit open scope (who may see and
// converse with it), enforced by the business layer. Admins and the owner
// always pass regardless of mode.
type VisibilitySpec struct {
	// Mode selects the scope: "all" opens the employee to every user,
	// "restricted" limits it to the listed teams and users.
	// +kubebuilder:validation:Enum=all;restricted
	Mode string `json:"mode"`
	// Teams lists AEP teams whose members may use the employee.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Teams []SubjectRef `json:"teams,omitempty"`
	// Users lists AEP users who may use the employee.
	// +optional
	// +kubebuilder:validation:MaxItems=500
	Users []SubjectRef `json:"users,omitempty"`
}

// SubjectRef references one AEP subject (team or user) by id with a
// display-name snapshot.
type SubjectRef struct {
	// ID is the AEP team or user id.
	ID string `json:"id"`
	// Name is the display name at assignment time.
	Name string `json:"name"`
}

// GovernanceSpec carries AEP governance middleware parameters.
type GovernanceSpec struct {
	AEPBaseURL   string `json:"aepBaseURL"`
	DeploymentID string `json:"deploymentID"`
	// DeploymentName is the AEP deployment's verified display name.
	// +optional
	DeploymentName string `json:"deploymentName,omitempty"`
	DefaultAEPUser string `json:"defaultAEPUser"`
	// +optional
	IdentitySource string `json:"identitySource,omitempty"`
	// +optional
	GovernedTools []string `json:"governedTools,omitempty"`
	// +optional
	TeamNames map[string]string `json:"teamNames,omitempty"`
	// +optional
	APIServiceCreds string `json:"apiServiceCreds,omitempty"`
}

// CredentialsRef names the platform Secret holding shared credentials
// (DEER model key, WeKnora MCP bearer, OpenViking root key, AEP service
// credentials). Secrets are never embedded in the CR.
type CredentialsRef struct {
	// Name of the Secret in the employee namespace.
	Name string `json:"name"`
}

// A2ASpec exposes the employee over the AEP A2A delegation contract. When
// enabled the operator renders a dedicated adapter instance fronting this
// employee's runtime: peer digital employees may delegate tasks to it with
// act-as semantics, chain-depth limits, and audit.
type A2ASpec struct {
	// Image of the a2a-adapter container.
	Image string `json:"image"`
	// MaxDepth is the delegation chain depth limit (default 3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxDepth int32 `json:"maxDepth,omitempty"`
	// NodePort exposes the adapter; 0 means ClusterIP only.
	// +optional
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePort int32 `json:"nodePort,omitempty"`
}

// ChannelsSpec toggles IM channel providers on the runtime. Channel
// credentials (bot id/secret, tokens) are supplied at runtime through the
// gateway's admin channel-connections API and never appear in the CR or
// the rendered config.
type ChannelsSpec struct {
	// Wecom enables the WeCom (企业微信) smart-bot channel.
	// +optional
	Wecom bool `json:"wecom,omitempty"`
	// WecomName is the bot's display name on WeCom. @-mentions of this name
	// address the bot itself; the governance middleware tells the agent so
	// it never mistakes its own name for a delegation target.
	// +optional
	WecomName string `json:"wecomName,omitempty"`
	// Feishu enables the Feishu channel.
	// +optional
	Feishu bool `json:"feishu,omitempty"`
	// FeishuName is the bot's display name in Feishu groups. The channel
	// resolves its own bot open_id at startup and matches @-mentions by that
	// id; this name is the explicit fallback used when the lookup is
	// unavailable (or when bot_open_id is set manually). With neither, a
	// mention is never treated as addressing the bot.
	// +optional
	FeishuName string `json:"feishuName,omitempty"`
	// FeishuDomain overrides the Feishu open-platform endpoint the channel
	// talks to. Empty keeps the channel default (https://open.feishu.cn).
	// Point it at a protocol-level mock or a self-hosted gateway to exercise
	// the channel without real credentials.
	// +optional
	FeishuDomain string `json:"feishuDomain,omitempty"`
	// WecomWSURL overrides the WeCom bot websocket endpoint. Empty keeps the
	// SDK default (wss://openws.work.weixin.qq.com). The aibot client always
	// passes a TLS context, so only wss:// endpoints connect; a plain ws://
	// mock needs a TLS front end in front of it.
	// +optional
	WecomWSURL string `json:"wecomWsUrl,omitempty"`
}

// PersistenceSpec controls durable runtime storage. The runtime home
// (/var/lib/deerflow: conversation threads, channel credentials, auth DB)
// defaults to a PVC so pod restarts and image upgrades no longer wipe
// conversation history. Set enabled=false to fall back to an EmptyDir.
type PersistenceSpec struct {
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`
	// Size of the home volume (minimum 1Gi).
	// +optional
	// +kubebuilder:default="2Gi"
	Size string `json:"size,omitempty"`
}

// ConversationStoreSpec selects where conversation threads are persisted.
// "sqlite" (default) stores per-employee on the home volume — single-writer,
// no multi-replica HA. "postgres" centralizes into PostgreSQL: each employee
// gets its own database (de_<name> by default, DSN rendered into a child
// Secret as DE_CONVERSATION_DB_URL) so runtime auth tables and threads are
// never shared across employees. The legacy platform-wide "shared" database
// was removed: it made the first employee own the global admin account.
type ConversationStoreSpec struct {
	// +kubebuilder:validation:Enum=sqlite;postgres
	// +optional
	// +kubebuilder:default=sqlite
	Backend string `json:"backend,omitempty"`
	// Physical database selection when Backend=postgres. Empty provisions a
	// per-employee database (de_<name>); any other value names an explicit
	// database the operator creates on boot. The reserved word "shared" is
	// rejected at reconciliation (the legacy shared database is gone).
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,40}$`
	// +optional
	Database string `json:"database,omitempty"`
}

// DigitalEmployeeSpec defines the desired state of a digital employee.
type DigitalEmployeeSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Description is the business-layer purpose statement (用途说明);
	// the operator never interprets it.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Description string `json:"description,omitempty"`

	Runtime RuntimeSpec `json:"runtime"`
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`
	// +optional
	Knowledge *KnowledgeSpec `json:"knowledge,omitempty"`

	Governance GovernanceSpec `json:"governance"`

	// Credentials references the shared platform secret.
	Credentials CredentialsRef `json:"credentials"`

	// Channels toggles IM channel providers; credentials arrive at runtime
	// via the gateway admin API, not through the CR.
	// +optional
	Channels *ChannelsSpec `json:"channels,omitempty"`

	// Persistence makes the runtime home durable (threads, channel
	// credentials, auth DB survive pod restarts and image upgrades).
	// +optional
	Persistence PersistenceSpec `json:"persistence,omitempty"`

	// ConversationStore centralizes conversation persistence (postgres)
	// or keeps it per-employee on the home volume (sqlite, default).
	// +optional
	ConversationStore ConversationStoreSpec `json:"conversationStore,omitempty"`

	// Owner is the AEP user id of the employee's owner (set by the business
	// layer; the operator never interprets it).
	// +optional
	Owner string `json:"owner,omitempty"`
	// OwnerName is the owner's AEP username for display.
	// +optional
	OwnerName string `json:"ownerName,omitempty"`
	// Team scopes the employee to a department. Members of this AEP team
	// can see and converse with the employee in addition to the owner;
	// admins bypass all team scoping. Empty means visible to everyone
	// (legacy behavior for employees created before team scoping).
	// +optional
	Team string `json:"team,omitempty"`
	// TeamName is the owning team's verified AEP display-name snapshot.
	// +optional
	TeamName string `json:"teamName,omitempty"`

	// Skills are the AEP skills enabled for this employee (set by the
	// business layer; the operator never interprets them). Each entry
	// pins the skill and the published version chosen at creation.
	// +optional
	// +kubebuilder:validation:MaxItems=200
	Skills []SkillRef `json:"skills,omitempty"`
	// Visibility is the employee's explicit open scope (who may see and
	// converse with it), enforced by the business layer. Nil keeps the
	// legacy owner/team rules.
	// +optional
	Visibility *VisibilitySpec `json:"visibility,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas,omitempty"`
	// NodePort exposes the runtime; 0 means ClusterIP only.
	// +optional
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePort int32 `json:"nodePort,omitempty"`
	// A2A optionally exposes this employee over the A2A delegation contract.
	// +optional
	A2A *A2ASpec `json:"a2a,omitempty"`
	// Email optionally gives this employee its own mailbox(es). The secret
	// named here must carry EMAIL_MAILBOXES (JSON array of mailbox configs
	// incl. credentials); the operator envFrom-injects it into the runtime
	// and the governance middleware registers the email tools when the env
	// is present. Portal is the writer of that secret.
	// +optional
	Email *EmailSpec `json:"email,omitempty"`
	// Hibernation optionally lets the operator scale the runtime to zero
	// when idle (KEDA watches the portal database's wake/activity tables).
	// Disabled by default; employees with an IM channel that holds an
	// outbound connection (WeCom) must stay resident until the
	// channel-gateway lands.
	// +optional
	Hibernation *HibernationSpec `json:"hibernation,omitempty"`
}

// EmailSpec references the employee's mailbox configuration secret.
type EmailSpec struct {
	// SecretName names a Secret in this namespace whose EMAIL_MAILBOXES key
	// holds the JSON mailbox array (label/address/imap/smtp/username/password).
	SecretName string `json:"secretName"`
}

// HibernationSpec configures scale-to-zero for one employee. The effective
// idle window = spec.hibernation.idleTimeout, falling back to the
// operator's HIBERNATION_IDLE_TIMEOUT default.
type HibernationSpec struct {
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// IdleTimeout is how long the employee stays up after the last
	// activity before scaling to zero (e.g. "30m", "2h").
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+[smh]$`
	IdleTimeout string `json:"idleTimeout,omitempty"`
}

// DigitalEmployeePhase describes the reconciliation phase.
// +kubebuilder:validation:Enum=Pending;Ready;Sleeping;Error
type DigitalEmployeePhase string

const (
	PhasePending  DigitalEmployeePhase = "Pending"
	PhaseReady    DigitalEmployeePhase = "Ready"
	PhaseSleeping DigitalEmployeePhase = "Sleeping"
	PhaseError    DigitalEmployeePhase = "Error"
)

// DigitalEmployeeStatus defines the observed state of a digital employee.
type DigitalEmployeeStatus struct {
	// +optional
	Phase DigitalEmployeePhase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// MemoryUser is the OpenViking user provisioned for this employee.
	// +optional
	MemoryUser string `json:"memoryUser,omitempty"`
	// Message carries the last error detail when Phase == Error.
	// +optional
	Message string `json:"message,omitempty"`
	// RuntimeAccount is the bootstrap state of the employee's runtime
	// account: the per-instance DeerFlow admin plus the owner account and
	// PAT provisioned through the runtime auth API (see the operator's
	// ensureRuntimeAccount). Ready means the PAT in the
	// de-<name>-runtime-creds Secret authenticates against the runtime.
	// +optional
	RuntimeAccount RuntimeAccountPhase `json:"runtimeAccount,omitempty"`
	// RuntimeAccountMessage carries the last runtime-account detail
	// (failure reason, upgrade hint, …).
	// +optional
	RuntimeAccountMessage string `json:"runtimeAccountMessage,omitempty"`
}

// RuntimeAccountPhase tracks the runtime-account bootstrap.
// +kubebuilder:validation:Enum=Pending;Ready;Failed;Unsupported
type RuntimeAccountPhase string

const (
	// RuntimeAccountPending: the runtime is not reachable yet; the
	// controller requeues and retries.
	RuntimeAccountPending RuntimeAccountPhase = "Pending"
	// RuntimeAccountReady: admin + owner account + PAT provisioned and the
	// PAT verifies against the runtime.
	RuntimeAccountReady RuntimeAccountPhase = "Ready"
	// RuntimeAccountFailed: manual intervention required (rejected
	// credentials, orphaned account, …).
	RuntimeAccountFailed RuntimeAccountPhase = "Failed"
	// RuntimeAccountUnsupported: the runtime image predates the provision
	// API; upgrade the image to get runtime accounts.
	RuntimeAccountUnsupported RuntimeAccountPhase = "Unsupported"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.runtime.model`
// +kubebuilder:printcolumn:name="MemoryUser",type=string,JSONPath=`.status.memoryUser`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.status.runtimeAccount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=de

// DigitalEmployee is one governed agent instance; the CR name is the
// business ID that memory and knowledge bind to.
type DigitalEmployee struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DigitalEmployeeSpec   `json:"spec,omitempty"`
	Status DigitalEmployeeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DigitalEmployeeList contains a list of DigitalEmployee.
type DigitalEmployeeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DigitalEmployee `json:"items"`
}

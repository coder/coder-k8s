package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CoderMCPServerConditionReady is True when Coder holds the MCP server
	// config as the spec describes.
	CoderMCPServerConditionReady = "Ready"
	// CoderMCPServerConditionReconciling is True while the controller works
	// toward the spec.
	CoderMCPServerConditionReconciling = "Reconciling"
	// CoderMCPServerConditionStalled is True when the controller stops until
	// the spec or an input changes.
	CoderMCPServerConditionStalled = "Stalled"

	// CoderMCPServerCleanupFinalizer keeps an MCP server until its Coder
	// config is deleted.
	CoderMCPServerCleanupFinalizer = "coder.com/mcp-server-cleanup"
)

// CoderMCPServerSpec defines one MCP server config in a Coder organization.
type CoderMCPServerSpec struct {
	// ControlPlaneRef names the CoderControlPlane in the same namespace. The
	// controller calls Coder with that control plane's operator token.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="controlPlaneRef is immutable"
	ControlPlaneRef CoderControlPlaneReference `json:"controlPlaneRef"`
	// Organization is the name of the Coder organization that holds the
	// config. Coder cannot move a config to another organization.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="organization is immutable"
	Organization string `json:"organization"`
	// Slug is the Coder slug of the config, unique in its organization. The
	// model sees the server's tools as <slug>__<tool>.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+(-[a-z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="slug is immutable"
	Slug string `json:"slug"`
	// DisplayName is the name that Coder shows to users in chat.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^\S([\s\S]*\S)?$`
	DisplayName string `json:"displayName"`
	// Description is a short summary of what the server provides.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^\S([\s\S]*\S)?$`
	// +optional
	Description string `json:"description,omitempty"`
	// IconURL is the icon URL, path or emoji that Coder shows.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^\S+$`
	// +optional
	IconURL string `json:"iconURL,omitempty"`
	// URL is the http:// or https:// MCP endpoint that Coder connects to. It
	// must not hold user info. Every Secret in auth must list its origin as an
	// allowed destination.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^\S+$`
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getHostname() != ''",message="url must be a valid URL with a host"
	// +kubebuilder:validation:XValidation:rule="self.matches('^https?://[^/?#@\\\\s]+([/?#].*)?$')",message="url must be an http:// or https:// URL with a host and no user info"
	URL string `json:"url"`
	// Transport is the MCP transport that Coder uses.
	// +kubebuilder:validation:Enum=streamable_http;sse
	// +kubebuilder:default=streamable_http
	// +optional
	Transport string `json:"transport,omitempty"`
	// Availability decides how chats offer the server. force_on adds it to
	// every new chat of a user who can read it. Coder's web UI preselects
	// default_on servers. The Coder server treats default_on and default_off
	// alike.
	// +kubebuilder:validation:Enum=force_on;default_on;default_off
	// +kubebuilder:default=default_off
	// +optional
	Availability string `json:"availability,omitempty"`
	// Enabled turns the server on or off in Coder. Chats never connect to a
	// disabled server. When omitted, the default is true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// AllowInPlanMode offers the server in plan-mode chats.
	// +optional
	AllowInPlanMode bool `json:"allowInPlanMode,omitempty"`
	// ModelIntent asks the model to state its intent with each tool call.
	// +optional
	ModelIntent bool `json:"modelIntent,omitempty"`
	// ForwardCoderHeaders sends the chat owner, chat and workspace IDs to the
	// server as X-Coder-* headers. Coder sends no token in them.
	// +optional
	ForwardCoderHeaders bool `json:"forwardCoderHeaders,omitempty"`
	// Auth is how Coder authenticates to the server.
	Auth CoderMCPServerAuth `json:"auth"`
	// Access, when set, makes the controller own the server's access list in
	// Coder: exactly the listed groups can read the server. When omitted, the
	// controller leaves the access list alone, and Coder's default lets every
	// organization member read the server.
	// +optional
	Access *CoderMCPServerAccess `json:"access,omitempty"`
}

// CoderMCPServerAuth is how Coder authenticates to the MCP server. Exactly
// the block that matches type is set.
// +kubebuilder:validation:XValidation:rule="(self.type == 'api_key') == has(self.apiKey)",message="auth.apiKey must be set exactly when auth.type is api_key"
// +kubebuilder:validation:XValidation:rule="(self.type == 'custom_headers') == has(self.customHeaders)",message="auth.customHeaders must be set exactly when auth.type is custom_headers"
// +kubebuilder:validation:XValidation:rule="(self.type == 'oauth2') == has(self.oauth2)",message="auth.oauth2 must be set exactly when auth.type is oauth2"
type CoderMCPServerAuth struct {
	// Type is the auth type. Coder's user_oidc type is not supported, because
	// it sends each user's login token to the server URL.
	// +kubebuilder:validation:Enum=none;api_key;custom_headers;oauth2
	Type string `json:"type"`
	// APIKey sends one Secret value in one header.
	// +optional
	APIKey *CoderMCPServerAPIKeyAuth `json:"apiKey,omitempty"`
	// CustomHeaders sends each Secret value in its own header.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(a, self.filter(b, b.name.lowerAscii() == a.name.lowerAscii()).size() == 1)",message="auth.customHeaders names must be unique, ignoring case"
	// +optional
	CustomHeaders []CoderMCPServerHeader `json:"customHeaders,omitempty"`
	// OAuth2 lets each user connect with an OAuth2 client that the admin
	// registered at the authorization server.
	// +optional
	OAuth2 *CoderMCPServerOAuth2Auth `json:"oauth2,omitempty"`
}

// CoderMCPServerAPIKeyAuth sends one Secret value in one header. Coder sends
// the value as it is, so a bearer token is stored as "Bearer <token>".
type CoderMCPServerAPIKeyAuth struct {
	// Header is the HTTP header name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[!#$%&'*+.^_\x60|~0-9A-Za-z-]+$`
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['host', 'content-length', 'content-type', 'content-encoding', 'transfer-encoding', 'connection', 'keep-alive', 'upgrade', 'te', 'trailer', 'accept', 'accept-encoding', 'proxy-authorization', 'proxy-connection', 'mcp-session-id', 'mcp-protocol-version', 'last-event-id']) && !self.lowerAscii().startsWith('x-coder-')",message="header is reserved for the HTTP transport, MCP, or Coder"
	Header string `json:"header"`
	// SecretRef references the key in a Secret in the same namespace that
	// holds the header value. The Secret must opt in to the credential Secret
	// contract and list the origin of spec.url.
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="secretRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="has(self.key) && size(self.key) > 0",message="secretRef.key must not be empty"
	SecretRef SecretKeySelector `json:"secretRef"`
}

// CoderMCPServerHeader is one header whose value comes from a Secret.
type CoderMCPServerHeader struct {
	// Name is the HTTP header name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[!#$%&'*+.^_\x60|~0-9A-Za-z-]+$`
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['host', 'content-length', 'content-type', 'content-encoding', 'transfer-encoding', 'connection', 'keep-alive', 'upgrade', 'te', 'trailer', 'accept', 'accept-encoding', 'proxy-authorization', 'proxy-connection', 'mcp-session-id', 'mcp-protocol-version', 'last-event-id']) && !self.lowerAscii().startsWith('x-coder-')",message="header is reserved for the HTTP transport, MCP, or Coder"
	Name string `json:"name"`
	// SecretRef references the key in a Secret in the same namespace that
	// holds the header value. The Secret must opt in to the credential Secret
	// contract and list the origin of spec.url.
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="secretRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="has(self.key) && size(self.key) > 0",message="secretRef.key must not be empty"
	SecretRef SecretKeySelector `json:"secretRef"`
}

// CoderMCPServerOAuth2Auth is a manually registered OAuth2 client. Coder's
// automatic discovery and dynamic client registration are not supported.
type CoderMCPServerOAuth2Auth struct {
	// ClientID is the OAuth2 client ID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^\S+$`
	ClientID string `json:"clientID"`
	// ClientSecretRef references the key in a Secret in the same namespace
	// that holds the client secret. The Secret must opt in to the credential
	// Secret contract and list the origins of spec.url, authURL, tokenURL and
	// revocationURL.
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="clientSecretRef.name must not be empty"
	// +kubebuilder:validation:XValidation:rule="has(self.key) && size(self.key) > 0",message="clientSecretRef.key must not be empty"
	ClientSecretRef SecretKeySelector `json:"clientSecretRef"`
	// AuthURL is the authorization endpoint that users' browsers open.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^\S+$`
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getHostname() != ''",message="authURL must be a valid URL with a host"
	// +kubebuilder:validation:XValidation:rule="self.matches('^https?://[^/?#@\\\\s]+([/?#].*)?$')",message="authURL must be an http:// or https:// URL with a host and no user info"
	AuthURL string `json:"authURL"`
	// TokenURL is the token endpoint that Coder sends the client secret to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^\S+$`
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getHostname() != ''",message="tokenURL must be a valid URL with a host"
	// +kubebuilder:validation:XValidation:rule="self.matches('^https?://[^/?#@\\\\s]+([/?#].*)?$')",message="tokenURL must be an http:// or https:// URL with a host and no user info"
	TokenURL string `json:"tokenURL"`
	// RevocationURL is the https:// token revocation endpoint. Coder sends the
	// client secret there when a user disconnects.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^\S+$`
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getHostname() != ''",message="revocationURL must be a valid URL with a host"
	// +kubebuilder:validation:XValidation:rule="self.matches('^https://[^/?#@\\\\s]+([/?#].*)?$')",message="revocationURL must be an https:// URL with a host and no user info"
	// +optional
	RevocationURL string `json:"revocationURL,omitempty"`
	// Scopes are the OAuth2 scopes that Coder requests.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[!#-\[\]-~]+$`
	// +listType=set
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// CoderMCPServerAccess lists the Coder groups that can read the server.
type CoderMCPServerAccess struct {
	// Groups are Coder group names in the server's organization. Everyone is
	// the group of all organization members. An empty list leaves the server
	// to organization admins.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=255
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`
	// +listType=set
	// +optional
	Groups []string `json:"groups,omitempty"`
}

// CoderMCPServerCredentialStatus records which Secret value was last sent to
// Coder for one credential field. It holds no secret data and nothing derived
// from it.
type CoderMCPServerCredentialStatus struct {
	// Field names the credential: apiKey, oauth2ClientSecret, or
	// header:<name>.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=160
	Field string `json:"field"`
	// SecretName is the Secret name that was read.
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// SecretKey is the key in the Secret that was read.
	// +optional
	SecretKey string `json:"secretKey,omitempty"`
	// SecretUID is the UID of the Secret that was read.
	// +optional
	SecretUID string `json:"secretUID,omitempty"`
	// SecretResourceVersion is the resourceVersion of the Secret that was read.
	// +optional
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`
}

// CoderMCPServerStatus defines the observed state of a CoderMCPServer.
type CoderMCPServerStatus struct {
	// ObservedGeneration is the generation the controller last processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AppliedGeneration is the last generation whose spec Coder fully holds.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`
	// OrganizationID is the Coder organization ID that spec.organization
	// resolved to first.
	// +optional
	OrganizationID string `json:"organizationID,omitempty"`
	// ConfigID is the Coder MCP server config ID.
	// +optional
	ConfigID string `json:"configID,omitempty"`
	// CreateAttemptTime is set before every create request. While it is set
	// and ConfigID is empty, a create request can be in flight or already
	// committed in Coder.
	// +optional
	CreateAttemptTime *metav1.Time `json:"createAttemptTime,omitempty"`
	// Credentials record the Secret values last sent to Coder.
	// +listType=map
	// +listMapKey=field
	// +optional
	Credentials []CoderMCPServerCredentialStatus `json:"credentials,omitempty"`
	// Conditions are Ready, Reconciling, and Stalled.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=codermcpservers,scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Organization",type=string,JSONPath=`.spec.organization`
// +kubebuilder:printcolumn:name="Slug",type=string,JSONPath=`.spec.slug`
// +kubebuilder:printcolumn:name="Auth",type=string,JSONPath=`.spec.auth.type`
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CoderMCPServer manages one MCP server config in a Coder organization.
type CoderMCPServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderMCPServerSpec   `json:"spec"`
	Status CoderMCPServerStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// CoderMCPServerList contains a list of CoderMCPServer objects.
type CoderMCPServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CoderMCPServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CoderMCPServer{}, &CoderMCPServerList{})
}

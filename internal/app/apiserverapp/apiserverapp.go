// Package apiserverapp provides the aggregated API server application mode for coder-k8s.
package apiserverapp

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/authentication/request/anonymous"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	apiserveropenapi "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	apiservercompatibility "k8s.io/apiserver/pkg/util/compatibility"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	openapiutil "k8s.io/kube-openapi/pkg/util"
	"k8s.io/kube-openapi/pkg/validation/spec"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/storage"
)

const (
	// DefaultSecureServingPort is the secure serving port used by aggregated-apiserver mode.
	DefaultSecureServingPort = 6443
	serverName               = "coder-k8s-aggregated-apiserver"
	// defaultRequestTimeout keeps API request lifetimes aligned with template build wait limits.
	defaultRequestTimeout = storage.MaxTemplateVersionBuildWaitTimeout
)

// Options configures aggregated-apiserver bootstrap behavior.
type Options struct {
	// SecureServingPort used when Listener is nil. Default: DefaultSecureServingPort.
	SecureServingPort int
	// Listener allows tests to bind to 127.0.0.1:0.
	Listener net.Listener
	// CoderURL is an optional fallback URL when CoderControlPlane status has no URL.
	CoderURL string
	// CoderSessionToken is the admin session token.
	CoderSessionToken string
	// CoderNamespace restricts the provider to serve only this namespace.
	// When non-empty, requests to other namespaces are rejected.
	CoderNamespace string
	// CoderRequestTimeout for SDK calls. Default 30s.
	CoderRequestTimeout time.Duration
	// ClientProvider overrides the default static provider.
	// When set, CoderURL/CoderSessionToken/CoderNamespace flags are ignored.
	ClientProvider coder.ClientProvider
}

type errClientProvider struct {
	serviceUnavailableMessage string
}

var _ coder.ClientProvider = (*errClientProvider)(nil)

func (p *errClientProvider) ClientForNamespace(ctx context.Context, _ string) (*codersdk.Client, error) {
	if p == nil {
		return nil, fmt.Errorf("assertion failed: err client provider must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if p.serviceUnavailableMessage == "" {
		return nil, fmt.Errorf("assertion failed: service unavailable message must not be empty")
	}

	return nil, apierrors.NewServiceUnavailable(p.serviceUnavailableMessage)
}

func buildClientProvider(opts Options, requestTimeout time.Duration) (coder.ClientProvider, error) {
	if requestTimeout <= 0 {
		return nil, fmt.Errorf("assertion failed: request timeout must be positive")
	}

	coderURL := strings.TrimSpace(opts.CoderURL)
	sessionToken := strings.TrimSpace(opts.CoderSessionToken)
	missing := make([]string, 0, 2)
	if coderURL == "" {
		missing = append(missing, "coder URL")
	}
	if sessionToken == "" {
		missing = append(missing, "coder session token")
	}
	if len(missing) > 0 {
		message := fmt.Sprintf(
			"coder client provider is not configured: missing %s; configure --coder-url and --coder-session-token",
			strings.Join(missing, " and "),
		)
		if len(missing) == 2 {
			return &errClientProvider{serviceUnavailableMessage: message}, nil
		}

		return nil, fmt.Errorf("coder client provider is partially configured: %s", message)
	}

	coderNamespace := strings.TrimSpace(opts.CoderNamespace)
	if coderNamespace == "" {
		return nil, fmt.Errorf("coder client provider namespace is not configured: configure --coder-namespace")
	}

	parsedCoderURL, err := url.Parse(coderURL)
	if err != nil {
		return nil, fmt.Errorf("parse coder URL %q: %w", coderURL, err)
	}
	if parsedCoderURL == nil {
		return nil, fmt.Errorf("assertion failed: parsed coder URL must not be nil")
	}

	provider, err := coder.NewStaticClientProvider(
		coder.Config{
			CoderURL:       parsedCoderURL,
			SessionToken:   sessionToken,
			RequestTimeout: requestTimeout,
		},
		coderNamespace,
	)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("assertion failed: coder client provider is nil after successful construction")
	}

	return provider, nil
}

// NewScheme builds the runtime scheme used by the aggregated API server.
func NewScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	// Register meta types for the v1 options external version used by
	// genericapiserver.NewDefaultAPIGroupInfo (OptionsExternalVersion "v1").
	// Without this, InstallAPIGroup fails with "no kind ListOptions is
	// registered for version v1".
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	utilruntime.Must(metainternalversion.AddToScheme(scheme))
	utilruntime.Must(aggregationv1alpha1.AddToScheme(scheme))

	// Register aggregation types for the internal hub version so the generic API
	// server can convert SSA requests between v1alpha1 and __internal.
	aggregationInternalGroupVersion := schema.GroupVersion{
		Group:   aggregationv1alpha1.SchemeGroupVersion.Group,
		Version: runtime.APIVersionInternal,
	}
	scheme.AddKnownTypes(
		aggregationInternalGroupVersion,
		&aggregationv1alpha1.CoderWorkspace{},
		&aggregationv1alpha1.CoderWorkspaceList{},
		&aggregationv1alpha1.CoderTemplate{},
		&aggregationv1alpha1.CoderTemplateList{},
	)

	return scheme
}

// NewRecommendedConfig builds a recommended generic API server config.
func NewRecommendedConfig(
	scheme *runtime.Scheme,
	codecs serializer.CodecFactory,
	secureServingOptions *genericoptions.SecureServingOptions,
) (*genericapiserver.RecommendedConfig, error) {
	if scheme == nil {
		return nil, fmt.Errorf("assertion failed: scheme must not be nil")
	}
	if secureServingOptions == nil {
		return nil, fmt.Errorf("assertion failed: secure serving options must not be nil")
	}

	recommendedConfig := genericapiserver.NewRecommendedConfig(codecs)
	if recommendedConfig == nil {
		return nil, fmt.Errorf("assertion failed: recommended config is nil after successful construction")
	}

	if err := secureServingOptions.MaybeDefaultWithSelfSignedCerts("localhost", []string{"localhost"}, nil); err != nil {
		return nil, fmt.Errorf("configure self-signed serving certs: %w", err)
	}
	if err := secureServingOptions.WithLoopback().ApplyTo(&recommendedConfig.SecureServing, &recommendedConfig.LoopbackClientConfig); err != nil {
		return nil, fmt.Errorf("configure secure serving: %w", err)
	}
	if recommendedConfig.SecureServing == nil {
		return nil, fmt.Errorf("assertion failed: secure serving config is nil after successful ApplyTo")
	}
	if recommendedConfig.LoopbackClientConfig == nil {
		return nil, fmt.Errorf("assertion failed: loopback client config is nil after successful ApplyTo")
	}

	authz := authorizerfactory.NewAlwaysAllowAuthorizer()
	recommendedConfig.Authentication = genericapiserver.AuthenticationInfo{
		Authenticator: anonymous.NewAuthenticator(nil),
	}
	recommendedConfig.Authorization = genericapiserver.AuthorizationInfo{
		Authorizer: authz,
	}
	recommendedConfig.RuleResolver = authz
	recommendedConfig.EffectiveVersion = apiservercompatibility.DefaultBuildEffectiveVersion()
	recommendedConfig.SkipOpenAPIInstallation = true
	recommendedConfig.RequestTimeout = defaultRequestTimeout

	definitionNamer := apiserveropenapi.NewDefinitionNamer(scheme)
	recommendedConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getOpenAPIDefinitions, definitionNamer)
	recommendedConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(getOpenAPIDefinitions, definitionNamer)

	return recommendedConfig, nil
}

// NewAPIGroupInfo creates APIGroupInfo for the aggregation.coder.com API group.
func NewAPIGroupInfo(
	scheme *runtime.Scheme,
	codecs serializer.CodecFactory,
	provider coder.ClientProvider,
) (*genericapiserver.APIGroupInfo, error) {
	if scheme == nil {
		return nil, fmt.Errorf("assertion failed: scheme must not be nil")
	}
	if provider == nil {
		return nil, fmt.Errorf("assertion failed: coder client provider must not be nil")
	}

	parameterCodec := runtime.NewParameterCodec(scheme)
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(
		aggregationv1alpha1.SchemeGroupVersion.Group,
		scheme,
		parameterCodec,
		codecs,
	)
	apiGroupInfo.VersionedResourcesStorageMap[aggregationv1alpha1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"coderworkspaces": storage.NewWorkspaceStorage(provider),
		"codertemplates":  storage.NewTemplateStorage(provider),
	}
	return &apiGroupInfo, nil
}

// InstallAPIGroup installs an API group into a generic API server.
func InstallAPIGroup(server *genericapiserver.GenericAPIServer, apiGroupInfo *genericapiserver.APIGroupInfo) error {
	if server == nil {
		return fmt.Errorf("assertion failed: generic API server must not be nil")
	}
	if apiGroupInfo == nil {
		return fmt.Errorf("assertion failed: API group info must not be nil")
	}

	return server.InstallAPIGroup(apiGroupInfo)
}

// NewGenericAPIServer builds and configures a generic API server instance.
func NewGenericAPIServer(recommendedConfig *genericapiserver.RecommendedConfig) (*genericapiserver.GenericAPIServer, error) {
	if recommendedConfig == nil {
		return nil, fmt.Errorf("assertion failed: recommended config must not be nil")
	}

	server, err := recommendedConfig.Complete().New(serverName, genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, fmt.Errorf("construct generic API server: %w", err)
	}
	if server == nil {
		return nil, fmt.Errorf("assertion failed: generic API server is nil after successful construction")
	}

	return server, nil
}

// Run starts the aggregated API server application mode.
func Run(ctx context.Context) error {
	return RunWithOptions(ctx, Options{})
}

// RunWithOptions starts the aggregated API server application mode.
func RunWithOptions(ctx context.Context, opts Options) error {
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}
	if opts.CoderRequestTimeout < 0 {
		return fmt.Errorf("assertion failed: coder request timeout must not be negative")
	}

	requestTimeout := opts.CoderRequestTimeout
	if requestTimeout == 0 {
		requestTimeout = 30 * time.Second
	}

	var provider coder.ClientProvider
	if opts.ClientProvider != nil {
		provider = opts.ClientProvider
	} else {
		var err error
		provider, err = buildClientProvider(opts, requestTimeout)
		if err != nil {
			return fmt.Errorf("build coder client provider: %w", err)
		}
	}
	if provider == nil {
		return fmt.Errorf("assertion failed: coder client provider is nil after successful construction")
	}

	if errProvider, ok := provider.(*errClientProvider); ok {
		log.Printf("warning: %s", errProvider.serviceUnavailableMessage)
	}

	scheme := NewScheme()
	if scheme == nil {
		return fmt.Errorf("assertion failed: scheme is nil after successful construction")
	}

	codecs := serializer.NewCodecFactory(scheme)
	secureServingOptions := genericoptions.NewSecureServingOptions()
	secureServingPort := opts.SecureServingPort
	if secureServingPort == 0 {
		secureServingPort = DefaultSecureServingPort
	}
	if secureServingPort < 0 {
		return fmt.Errorf("assertion failed: secure serving port must not be negative")
	}
	secureServingOptions.BindPort = secureServingPort
	if opts.Listener != nil {
		secureServingOptions.Listener = opts.Listener
		secureServingOptions.BindPort = 0
	}
	secureServingOptions.ServerCert.CertDirectory = ""
	secureServingOptions.ServerCert.PairName = ""

	recommendedConfig, err := NewRecommendedConfig(scheme, codecs, secureServingOptions)
	if err != nil {
		return fmt.Errorf("configure aggregated API server: %w", err)
	}

	server, err := NewGenericAPIServer(recommendedConfig)
	if err != nil {
		return err
	}

	apiGroupInfo, err := NewAPIGroupInfo(scheme, codecs, provider)
	if err != nil {
		return fmt.Errorf("build API group info: %w", err)
	}
	if err := InstallAPIGroup(server, apiGroupInfo); err != nil {
		return fmt.Errorf("install API group: %w", err)
	}

	return server.PrepareRun().RunWithContext(ctx)
}

func getOpenAPIDefinitions(_ openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
	workspaceDefinitionName := openapiutil.GetCanonicalTypeName(&aggregationv1alpha1.CoderWorkspace{})
	workspaceListDefinitionName := openapiutil.GetCanonicalTypeName(&aggregationv1alpha1.CoderWorkspaceList{})
	templateDefinitionName := openapiutil.GetCanonicalTypeName(&aggregationv1alpha1.CoderTemplate{})
	templateListDefinitionName := openapiutil.GetCanonicalTypeName(&aggregationv1alpha1.CoderTemplateList{})

	groupVersionKindExtension := func(kind string) spec.VendorExtensible {
		return spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-group-version-kind": []interface{}{
					map[string]interface{}{
						"group":   aggregationv1alpha1.SchemeGroupVersion.Group,
						"version": aggregationv1alpha1.SchemeGroupVersion.Version,
						"kind":    kind,
					},
				},
			},
		}
	}

	boolSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}}
	dateTimeSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "date-time"}}
	int64Schema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"integer"}, Format: "int64"}}
	stringSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}
	objectMetaSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}}
	listMetaSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}}
	filesSchema := spec.Schema{
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-map-type": "atomic",
			},
		},
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			AdditionalProperties: &spec.SchemaOrBool{
				Allows: true,
				Schema: &stringSchema,
			},
		},
	}

	workspaceSchema := spec.Schema{
		VendorExtensible: groupVersionKindExtension("CoderWorkspace"),
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": stringSchema,
				"kind":       stringSchema,
				"metadata":   objectMetaSchema,
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"organization":      stringSchema,
							"templateName":      stringSchema,
							"templateVersionID": stringSchema,
							"running":           boolSchema,
							"ttlMillis":         int64Schema,
							"autostartSchedule": stringSchema,
						},
					},
				},
				"status": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"id":                stringSchema,
							"ownerName":         stringSchema,
							"organizationName":  stringSchema,
							"templateName":      stringSchema,
							"latestBuildID":     stringSchema,
							"latestBuildStatus": stringSchema,
							"autoShutdown":      dateTimeSchema,
							"lastUsedAt":        dateTimeSchema,
						},
					},
				},
			},
		},
	}

	templateSchema := spec.Schema{
		VendorExtensible: groupVersionKindExtension("CoderTemplate"),
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": stringSchema,
				"kind":       stringSchema,
				"metadata":   objectMetaSchema,
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"organization": stringSchema,
							"versionID":    stringSchema,
							"displayName":  stringSchema,
							"description":  stringSchema,
							"icon":         stringSchema,
							"files":        filesSchema,
							"running":      boolSchema,
						},
					},
				},
				"status": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"id":               stringSchema,
							"organizationName": stringSchema,
							"activeVersionID":  stringSchema,
							"deprecated":       boolSchema,
							"updatedAt":        dateTimeSchema,
							"autoShutdown":     dateTimeSchema,
						},
					},
				},
			},
		},
	}

	workspaceListSchema := spec.Schema{
		VendorExtensible: groupVersionKindExtension("CoderWorkspaceList"),
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": stringSchema,
				"kind":       stringSchema,
				"metadata":   listMetaSchema,
				"items": {
					SchemaProps: spec.SchemaProps{
						Type:  []string{"array"},
						Items: &spec.SchemaOrArray{Schema: &workspaceSchema},
					},
				},
			},
		},
	}

	templateListSchema := spec.Schema{
		VendorExtensible: groupVersionKindExtension("CoderTemplateList"),
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": stringSchema,
				"kind":       stringSchema,
				"metadata":   listMetaSchema,
				"items": {
					SchemaProps: spec.SchemaProps{
						Type:  []string{"array"},
						Items: &spec.SchemaOrArray{Schema: &templateSchema},
					},
				},
			},
		},
	}

	return map[string]openapicommon.OpenAPIDefinition{
		workspaceDefinitionName: {
			Schema: workspaceSchema,
		},
		workspaceListDefinitionName: {
			Schema: workspaceListSchema,
		},
		templateDefinitionName: {
			Schema: templateSchema,
		},
		templateListDefinitionName: {
			Schema: templateListSchema,
		},
	}
}

// Package apiserverapp provides the aggregated API server application mode for coder-k8s.
package apiserverapp

import (
	"context"
	"fmt"

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
	"github.com/coder/coder-k8s/internal/aggregated/storage"
)

const (
	// DefaultSecureServingPort is the secure serving port used by aggregated-apiserver mode.
	DefaultSecureServingPort = 6443
	serverName               = "coder-k8s-aggregated-apiserver"
)

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

	definitionNamer := apiserveropenapi.NewDefinitionNamer(scheme)
	recommendedConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getOpenAPIDefinitions, definitionNamer)
	recommendedConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(getOpenAPIDefinitions, definitionNamer)

	return recommendedConfig, nil
}

// NewAPIGroupInfo creates APIGroupInfo for the aggregation.coder.com API group.
func NewAPIGroupInfo(scheme *runtime.Scheme, codecs serializer.CodecFactory) (*genericapiserver.APIGroupInfo, error) {
	if scheme == nil {
		return nil, fmt.Errorf("assertion failed: scheme must not be nil")
	}

	parameterCodec := runtime.NewParameterCodec(scheme)
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(
		aggregationv1alpha1.SchemeGroupVersion.Group,
		scheme,
		parameterCodec,
		codecs,
	)
	apiGroupInfo.VersionedResourcesStorageMap[aggregationv1alpha1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"coderworkspaces": storage.NewWorkspaceStorage(),
		"codertemplates":  storage.NewTemplateStorage(),
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
	if ctx == nil {
		return fmt.Errorf("assertion failed: context must not be nil")
	}

	scheme := NewScheme()
	if scheme == nil {
		return fmt.Errorf("assertion failed: scheme is nil after successful construction")
	}

	codecs := serializer.NewCodecFactory(scheme)
	secureServingOptions := genericoptions.NewSecureServingOptions()
	secureServingOptions.BindPort = DefaultSecureServingPort
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

	apiGroupInfo, err := NewAPIGroupInfo(scheme, codecs)
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

	boolSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}}
	dateTimeSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "date-time"}}

	workspaceSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"running": boolSchema,
						},
					},
				},
				"status": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"autoShutdown": dateTimeSchema,
						},
					},
				},
			},
		},
	}

	templateSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"running": boolSchema,
						},
					},
				},
				"status": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"autoShutdown": dateTimeSchema,
						},
					},
				},
			},
		},
	}

	workspaceListSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
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
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
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

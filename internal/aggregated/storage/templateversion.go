package storage

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/convert"
	"github.com/coder/coder/v2/codersdk"
)

var (
	_ rest.Storage              = (*TemplateVersionStorage)(nil)
	_ rest.Getter               = (*TemplateVersionStorage)(nil)
	_ rest.Scoper               = (*TemplateVersionStorage)(nil)
	_ rest.SingularNameProvider = (*TemplateVersionStorage)(nil)
)

// TemplateVersionStorage serves read-only CoderTemplateVersion objects from codersdk. Versions
// change outside this server (Coder UI and CLI, imports, template updates), so it deliberately
// implements no watch and no write verbs.
type TemplateVersionStorage struct {
	provider coder.ClientProvider
}

// NewTemplateVersionStorage builds codersdk-backed storage for CoderTemplateVersion resources.
func NewTemplateVersionStorage(provider coder.ClientProvider) *TemplateVersionStorage {
	if provider == nil {
		panic("assertion failed: template version client provider must not be nil")
	}

	return &TemplateVersionStorage{provider: provider}
}

// New returns an empty CoderTemplateVersion object.
func (s *TemplateVersionStorage) New() runtime.Object {
	return &aggregationv1alpha1.CoderTemplateVersion{}
}

// Destroy is a no-op: the storage holds no background resources.
func (s *TemplateVersionStorage) Destroy() {}

// NamespaceScoped returns true because CoderTemplateVersion is namespaced.
func (s *TemplateVersionStorage) NamespaceScoped() bool {
	return true
}

// GetSingularName returns the singular name of the CoderTemplateVersion resource.
func (s *TemplateVersionStorage) GetSingularName() string {
	return "codertemplateversion"
}

// Get fetches a CoderTemplateVersion named <organization>.<template>.<version>. It makes three
// Coder requests (organization, template, version by name) and never downloads template source.
func (s *TemplateVersionStorage) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template version storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if name == "" {
		return nil, fmt.Errorf("assertion failed: template version name must not be empty")
	}

	namespace, badNamespaceErr := requiredNamespaceFromRequestContext(ctx)
	if badNamespaceErr != nil {
		return nil, badNamespaceErr
	}

	orgName, templateName, versionName, err := coder.ParseTemplateVersionName(name)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	// codersdk inserts names into request paths without escaping. Only path safety is checked here,
	// not Coder's name rules, so versions with names from older Coder releases stay readable; the
	// exact-name checks below catch any lookup that resolves to a different object.
	for _, segment := range []string{orgName, templateName, versionName} {
		if err := requirePathSafeSegment(name, segment); err != nil {
			return nil, err
		}
	}

	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}
	resource := aggregationv1alpha1.Resource("codertemplateversions")

	org, err := sdk.OrganizationByName(ctx, orgName)
	if err != nil {
		return nil, coder.MapCoderError(err, resource, name)
	}
	if err := requireCanonicalVersionSegment(name, "organization", orgName, org.Name,
		coder.BuildTemplateVersionName(org.Name, templateName, versionName)); err != nil {
		return nil, err
	}

	template, err := sdk.TemplateByName(ctx, org.ID, templateName)
	if err != nil {
		return nil, coder.MapCoderError(err, resource, name)
	}
	if template.OrganizationID != org.ID {
		return nil, fmt.Errorf("assertion failed: template %q must belong to organization %s", name, org.ID)
	}
	if err := requireCanonicalVersionSegment(name, "template", templateName, template.Name,
		coder.BuildTemplateVersionName(orgName, template.Name, versionName)); err != nil {
		return nil, err
	}

	version, err := sdk.TemplateVersionByName(ctx, template.ID, versionName)
	if err != nil {
		return nil, coder.MapCoderError(err, resource, name)
	}
	if version.TemplateID == nil || *version.TemplateID != template.ID {
		return nil, fmt.Errorf("assertion failed: template version %q must belong to template %s", name, template.ID)
	}
	// Coder looks up version names case-sensitively today; do not rely on it.
	if err := requireCanonicalVersionSegment(name, "version", versionName, version.Name,
		coder.BuildTemplateVersionName(orgName, templateName, version.Name)); err != nil {
		return nil, err
	}

	return convert.TemplateVersionToK8s(namespace, template, version), nil
}

// requirePathSafeSegment rejects a name segment that could change the Coder request path or query.
func requirePathSafeSegment(name, segment string) error {
	unsafe := segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\?#%")
	for _, r := range segment {
		if r <= ' ' || r == 0x7f {
			unsafe = true
		}
	}
	if !unsafe {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"invalid template version name %q: segment %q must not be \".\" or \"..\" and must not contain spaces, control characters, or any of / \\ ? # %%",
		name, segment,
	))
}

// requireCanonicalVersionSegment rejects a lookup whose result has a different name than the
// requested segment, for example an organization alias or alternate casing. The object must be
// returned under exactly the requested metadata.name.
func requireCanonicalVersionSegment(name, segmentKind, requested, resolved, canonicalName string) error {
	if resolved == "" {
		return fmt.Errorf("assertion failed: resolved %s for %q must have a name", segmentKind, name)
	}
	if resolved == requested {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"template version name %q: %s segment %q resolves to %s %q; use the canonical name %q",
		name, segmentKind, requested, segmentKind, resolved, canonicalName,
	))
}

func (s *TemplateVersionStorage) clientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("assertion failed: template version client provider must not be nil")
	}

	sdk, err := s.provider.ClientForNamespace(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("resolve codersdk client for namespace %q: %w", namespace, err)
	}
	if sdk == nil {
		return nil, fmt.Errorf("assertion failed: template version client provider returned nil codersdk client")
	}

	return sdk, nil
}

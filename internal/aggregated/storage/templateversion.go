package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder-k8s/internal/aggregated/convert"
	"github.com/coder/coder/v2/codersdk"
)

var (
	_ rest.Storage              = (*TemplateVersionStorage)(nil)
	_ rest.Getter               = (*TemplateVersionStorage)(nil)
	_ rest.Lister               = (*TemplateVersionStorage)(nil)
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

// NewList returns an empty CoderTemplateVersionList object.
func (s *TemplateVersionStorage) NewList() runtime.Object {
	return &aggregationv1alpha1.CoderTemplateVersionList{}
}

// List returns every version of every template, archived, failed and pending ones included. It makes
// one Coder request for the template list plus one per template, and never downloads template
// source. limit is ignored: the list is always complete and has no continue token.
func (s *TemplateVersionStorage) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	if s == nil {
		return nil, fmt.Errorf("assertion failed: template version storage must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if opts != nil {
		if opts.Continue != "" || opts.ResourceVersionMatch != "" || (opts.ResourceVersion != "" && opts.ResourceVersion != "0") {
			return nil, apierrors.NewBadRequest(
				"codertemplateversions lists are always complete and current: continue, resourceVersionMatch and resourceVersion other than \"0\" are not supported",
			)
		}
	}

	requestNamespace, err := namespaceFromRequestContext(ctx)
	if err != nil {
		return nil, err
	}
	filter, err := filterForListOptions(requestNamespace, opts)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}

	namespaces := []string{requestNamespace}
	lister, canFanOut := s.provider.(coder.NamespaceLister)
	switch {
	case requestNamespace == "" && canFanOut:
		if namespaces, err = lister.EligibleNamespaces(ctx); err != nil {
			return nil, err
		}
	case requestNamespace == "":
		responseNamespace, err := namespaceForListConversion(ctx, requestNamespace, s.provider)
		if err != nil {
			return nil, err
		}
		namespaces = []string{responseNamespace}
	}

	list := &aggregationv1alpha1.CoderTemplateVersionList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CoderTemplateVersionList",
			APIVersion: aggregationv1alpha1.SchemeGroupVersion.String(),
		},
		Items: make([]aggregationv1alpha1.CoderTemplateVersion, 0),
	}
	for _, namespace := range namespaces {
		items, err := s.listNamespace(ctx, namespace)
		if err != nil {
			return nil, err
		}
		for i := range items {
			if filter != nil {
				if _, keep := filter(watch.Event{Object: &items[i]}); !keep {
					continue
				}
			}
			list.Items = append(list.Items, items[i])
		}
	}

	sort.SliceStable(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		switch {
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		case a.Spec.Organization != b.Spec.Organization:
			return a.Spec.Organization < b.Spec.Organization
		case a.Spec.TemplateName != b.Spec.TemplateName:
			return a.Spec.TemplateName < b.Spec.TemplateName
		case !a.CreationTimestamp.Equal(&b.CreationTimestamp):
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		default:
			return a.UID < b.UID
		}
	})

	return list, nil
}

func (s *TemplateVersionStorage) listNamespace(ctx context.Context, namespace string) ([]aggregationv1alpha1.CoderTemplateVersion, error) {
	resource := aggregationv1alpha1.Resource("codertemplateversions")
	sdk, err := s.clientForNamespace(ctx, namespace)
	if err != nil {
		return nil, wrapClientError(err)
	}

	templates, err := sdk.Templates(ctx, codersdk.TemplateFilter{})
	if err != nil {
		return nil, coder.MapCoderError(err, resource, "<list>")
	}

	items := make([]aggregationv1alpha1.CoderTemplateVersion, 0)
	for _, template := range templates {
		versions, err := sdk.TemplateVersionsByTemplate(ctx, codersdk.TemplateVersionsByTemplateRequest{
			TemplateID:      template.ID,
			IncludeArchived: true,
		})
		if err != nil {
			var sdkErr *codersdk.Error
			if errors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusNotFound {
				continue // The template was deleted between the two requests.
			}
			return nil, coder.MapCoderError(err, resource, "<list>")
		}
		for _, version := range versions {
			if version.TemplateID == nil || *version.TemplateID != template.ID {
				return nil, fmt.Errorf("assertion failed: listed version %s must belong to template %s", version.ID, template.ID)
			}
			items = append(items, *convert.TemplateVersionToK8s(namespace, template, version))
		}
	}

	return items, nil
}

// ConvertToTable renders versions for kubectl. -o wide adds the version ID and message.
func (s *TemplateVersionStorage) ConvertToTable(_ context.Context, object, tableOptions runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{}
	addRow := func(v *aggregationv1alpha1.CoderTemplateVersion) {
		message, _, _ := strings.Cut(v.Spec.Message, "\n")
		if len(message) > 60 {
			message = message[:57] + "..."
		}
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				v.Name, v.Status.Active, v.Status.Job.Status, v.Status.Archived, v.Status.CreatedBy,
				humanAge(v.CreationTimestamp.Time), v.Status.ID, message,
			},
			Object: runtime.RawExtension{Object: v},
		})
	}

	switch typed := object.(type) {
	case *aggregationv1alpha1.CoderTemplateVersion:
		addRow(typed)
		table.ResourceVersion = typed.ResourceVersion
	case *aggregationv1alpha1.CoderTemplateVersionList:
		for i := range typed.Items {
			addRow(&typed.Items[i])
		}
	default:
		return nil, fmt.Errorf("assertion failed: cannot render %T as a codertemplateversions table", object)
	}

	if options, ok := tableOptions.(*metav1.TableOptions); !ok || !options.NoHeaders {
		table.ColumnDefinitions = []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Active", Type: "boolean"},
			{Name: "Job", Type: "string"},
			{Name: "Archived", Type: "boolean"},
			{Name: "Created By", Type: "string"},
			{Name: "Age", Type: "string"},
			{Name: "ID", Type: "string", Priority: 1},
			{Name: "Message", Type: "string", Priority: 1},
		}
	}

	return table, nil
}

// humanAge formats the time since t like kubectl's AGE column, in the largest whole unit.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	age := time.Since(t)
	switch {
	case age < 2*time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < 2*time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
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
	if err := requireCanonicalVersionSegment(name, "organization", orgName, org.Name, func() string {
		return coder.BuildTemplateVersionName(org.Name, templateName, versionName)
	}); err != nil {
		return nil, err
	}

	template, err := sdk.TemplateByName(ctx, org.ID, templateName)
	if err != nil {
		return nil, coder.MapCoderError(err, resource, name)
	}
	if template.OrganizationID != org.ID {
		return nil, fmt.Errorf("assertion failed: template %q must belong to organization %s", name, org.ID)
	}
	if err := requireCanonicalVersionSegment(name, "template", templateName, template.Name, func() string {
		return coder.BuildTemplateVersionName(orgName, template.Name, versionName)
	}); err != nil {
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
	if err := requireCanonicalVersionSegment(name, "version", versionName, version.Name, func() string {
		return coder.BuildTemplateVersionName(orgName, templateName, version.Name)
	}); err != nil {
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
// returned under exactly the requested metadata.name. canonicalName builds the hint for the error
// message; it runs only on a mismatch, after the resolved name is known to be non-empty.
func requireCanonicalVersionSegment(name, segmentKind, requested, resolved string, canonicalName func() string) error {
	if resolved == "" {
		return fmt.Errorf("assertion failed: resolved %s for %q must have a name", segmentKind, name)
	}
	if resolved == requested {
		return nil
	}

	return apierrors.NewBadRequest(fmt.Sprintf(
		"template version name %q: %s segment %q resolves to %s %q; use the canonical name %q",
		name, segmentKind, requested, segmentKind, resolved, canonicalName(),
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

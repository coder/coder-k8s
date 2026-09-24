package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	"github.com/coder/coder-k8s/internal/aggregated/coder"
	"github.com/coder/coder/v2/codersdk"
)

const (
	// templateVersionNamePrefix marks template versions that an Update names after its target and source.
	templateVersionNamePrefix = "k8s-"
	// templateVersionNameHashHexLen is the number of hex digits of the target+source hash in a version name.
	templateVersionNameHashHexLen = 20
	// maxTemplateVersionNameLookupsPerRequest bounds the version-by-name lookups one Update may make. Finding
	// the latest attempt costs about 2*log2(attempts)+1 lookups, so this covers thousands of earlier failed
	// attempts plus several lost duplicate-name races. A request that exhausts it fails with 503 and creates
	// nothing; the next request starts a fresh budget.
	maxTemplateVersionNameLookupsPerRequest = 48
)

// templateVersionBaseName returns "k8s-<20 hex>", derived from the target template and the exact source zip.
// Update retries with the same source recompute the same name, also after a restart; other templates never
// share it.
func templateVersionBaseName(templateID uuid.UUID, zipBytes []byte) (string, error) {
	if templateID == uuid.Nil {
		return "", fmt.Errorf("assertion failed: template ID must not be nil")
	}
	if len(zipBytes) == 0 {
		return "", fmt.Errorf("assertion failed: template source zip must not be empty")
	}

	sourceSum := sha256.Sum256(zipBytes)
	nameSum := sha256.New()
	_, _ = nameSum.Write([]byte("coder-k8s/template-version/v1\x00" + templateID.String() + "\x00"))
	_, _ = nameSum.Write(sourceSum[:])
	return templateVersionNamePrefix + hex.EncodeToString(nameSum.Sum(nil))[:templateVersionNameHashHexLen], nil
}

// templateVersionAttemptName returns the name of attempt n: the base name for n == 1, "<base>-<n>" after that.
func templateVersionAttemptName(baseName string, attempt int) (string, error) {
	if baseName == "" {
		return "", fmt.Errorf("assertion failed: template version base name must not be empty")
	}
	if attempt < 1 {
		return "", fmt.Errorf("assertion failed: template version attempt must be >= 1, got %d", attempt)
	}

	name := baseName
	if attempt > 1 {
		name = baseName + "-" + strconv.Itoa(attempt)
	}
	if err := codersdk.TemplateVersionNameValid(name); err != nil {
		return "", fmt.Errorf("assertion failed: derived template version name %q is invalid: %w", name, err)
	}
	return name, nil
}

// templateVersionReusable reports whether an existing attempt can still become the active version.
func templateVersionReusable(version codersdk.TemplateVersion) bool {
	if version.Archived {
		return false
	}
	switch version.Job.Status {
	case codersdk.ProvisionerJobPending, codersdk.ProvisionerJobRunning, codersdk.ProvisionerJobSucceeded:
		return true
	default:
		return false
	}
}

// templateVersionLookup looks attempt names up under one per-request budget.
type templateVersionLookup struct {
	sdk        *codersdk.Client
	templateID uuid.UUID
	baseName   string
	name       string // CoderTemplate name, for error mapping
	lookups    int
}

// get returns the template version named after attempt n, or found=false when Coder has none.
func (l *templateVersionLookup) get(ctx context.Context, attempt int) (codersdk.TemplateVersion, bool, error) {
	if err := ctx.Err(); err != nil {
		return codersdk.TemplateVersion{}, false, templateVersionLookupContextError(err)
	}
	if l.lookups >= maxTemplateVersionNameLookupsPerRequest {
		return codersdk.TemplateVersion{}, false, apierrors.NewServiceUnavailable(fmt.Sprintf(
			"could not settle on a template version for %q within %d lookups; retry the request",
			l.name, maxTemplateVersionNameLookupsPerRequest,
		))
	}
	l.lookups++

	attemptName, err := templateVersionAttemptName(l.baseName, attempt)
	if err != nil {
		return codersdk.TemplateVersion{}, false, err
	}
	version, err := l.sdk.TemplateVersionByName(ctx, l.templateID, attemptName)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return codersdk.TemplateVersion{}, false, templateVersionLookupContextError(ctxErr)
		}
		var sdkErr *codersdk.Error
		if errors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusNotFound {
			return codersdk.TemplateVersion{}, false, nil
		}
		return codersdk.TemplateVersion{}, false, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), l.name)
	}
	if version.ID == uuid.Nil || version.Name != attemptName {
		return codersdk.TemplateVersion{}, false, fmt.Errorf(
			"assertion failed: lookup of template version %q returned %q (%s)", attemptName, version.Name, version.ID,
		)
	}
	return version, true, nil
}

// templateVersionLookupContextError maps a request deadline or cancellation during the attempt lookup to
// 504, like the import wait does.
func templateVersionLookupContextError(ctxErr error) error {
	return apierrors.NewTimeoutError(fmt.Sprintf("looking up earlier template version attempts: %v", ctxErr), 0)
}

// latestAttempt returns the highest existing attempt number (0 when there is none) and its version. Attempts
// are created in order, only after the previous one became unusable, so existing names form the run 1..n: an
// exponential probe followed by a binary search finds n in O(log n) lookups.
func (l *templateVersionLookup) latestAttempt(ctx context.Context) (int, codersdk.TemplateVersion, error) {
	present, presentVersion := 0, codersdk.TemplateVersion{}
	absent := 1
	for {
		version, found, err := l.get(ctx, absent)
		if err != nil {
			return 0, codersdk.TemplateVersion{}, err
		}
		if !found {
			break
		}
		present, presentVersion = absent, version
		absent *= 2
	}
	for absent-present > 1 {
		middle := present + (absent-present)/2
		version, found, err := l.get(ctx, middle)
		if err != nil {
			return 0, codersdk.TemplateVersion{}, err
		}
		if found {
			present, presentVersion = middle, version
		} else {
			absent = middle
		}
	}
	return present, presentVersion, nil
}

// ensureTemplateVersionForUpdate returns the template version an Update should wait on for zipBytes. It
// reuses the latest attempt with the derived name while that attempt is pending, running or succeeded, and
// otherwise creates the next attempt ("<base>-<n+1>"). If another request creates that name first, Coder
// answers 409 and the lookup runs again, so concurrent retries converge on one version.
func ensureTemplateVersionForUpdate(
	ctx context.Context,
	sdk *codersdk.Client,
	organization string,
	templateID uuid.UUID,
	name string,
	zipBytes []byte,
) (codersdk.TemplateVersion, error) {
	if sdk == nil {
		return codersdk.TemplateVersion{}, fmt.Errorf("assertion failed: codersdk client must not be nil")
	}
	if organization == "" || name == "" {
		return codersdk.TemplateVersion{}, fmt.Errorf("assertion failed: organization and template name must not be empty")
	}

	baseName, err := templateVersionBaseName(templateID, zipBytes)
	if err != nil {
		return codersdk.TemplateVersion{}, err
	}
	lookup := &templateVersionLookup{sdk: sdk, templateID: templateID, baseName: baseName, name: name}

	var (
		fileID uuid.UUID
		orgID  uuid.UUID
	)
	for {
		attempt, version, err := lookup.latestAttempt(ctx)
		if err != nil {
			return codersdk.TemplateVersion{}, err
		}
		if attempt > 0 && templateVersionReusable(version) {
			return version, nil
		}

		nextName, err := templateVersionAttemptName(baseName, attempt+1)
		if err != nil {
			return codersdk.TemplateVersion{}, err
		}
		if fileID == uuid.Nil {
			uploadResponse, err := sdk.Upload(ctx, codersdk.ContentTypeZip, bytes.NewReader(zipBytes))
			if err != nil {
				return codersdk.TemplateVersion{}, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}
			if uploadResponse.ID == uuid.Nil {
				return codersdk.TemplateVersion{}, fmt.Errorf("assertion failed: uploaded file ID must not be nil")
			}
			org, err := sdk.OrganizationByName(ctx, organization)
			if err != nil {
				return codersdk.TemplateVersion{}, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
			}
			fileID, orgID = uploadResponse.ID, org.ID
		}

		created, err := sdk.CreateTemplateVersion(ctx, orgID, codersdk.CreateTemplateVersionRequest{
			Name:          nextName,
			TemplateID:    templateID,
			StorageMethod: codersdk.ProvisionerStorageMethodFile,
			FileID:        fileID,
			Provisioner:   codersdk.ProvisionerTypeTerraform,
		})
		if err != nil {
			if isDuplicateTemplateVersionName(err) {
				continue // another request created this attempt first: look it up and wait on it
			}
			return codersdk.TemplateVersion{}, coder.MapCoderError(err, aggregationv1alpha1.Resource("codertemplates"), name)
		}
		if created.ID == uuid.Nil {
			return codersdk.TemplateVersion{}, fmt.Errorf("assertion failed: new template version ID must not be nil")
		}
		return created, nil
	}
}

// isDuplicateTemplateVersionName reports Coder's 409 for an existing (template_id, name) pair
// (coderd/templateversions.go in Coder v2.37.2 marks it with a "name" validation error).
func isDuplicateTemplateVersionName(err error) bool {
	var sdkErr *codersdk.Error
	if !errors.As(err, &sdkErr) || sdkErr.StatusCode() != http.StatusConflict {
		return false
	}
	for _, validation := range sdkErr.Validations {
		if validation.Field == "name" {
			return true
		}
	}
	return false
}

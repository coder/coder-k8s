// Package controller contains Kubernetes controllers for coder-k8s resources.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
	"github.com/coder/coder-k8s/internal/controlplaneurl"
	"github.com/coder/coder-k8s/internal/credentialsecret"
	"github.com/coder/coder-k8s/internal/operatortoken"
)

const (
	defaultProvisionerReplicas                      = int32(1)
	defaultProvisionerTerminationGracePeriodSeconds = int64(600)
	defaultProvisionerOrganizationName              = "default"
	provisionerNamePrefix                           = "provisioner-"
	provisionerServiceAccountSuffix                 = "-provisioner"
	provisionerKeyChecksumAnnotation                = "checksum/provisioner-key"

	provisionerRateLimitBackoffBase  = 2 * time.Second
	provisionerRateLimitBackoffCap   = 2 * time.Minute
	provisionerRateLimitBackoffFloor = 1 * time.Second
	provisionerRateLimitJitterRatio  = 0.2

	externalProvisionerEntitlementRetryInterval = 2 * time.Minute
	provisionerOperatorAccessRetryInterval      = 15 * time.Second

	// #nosec G101 -- this is a field index key, not a credential.
	provisionerControlPlaneRefNameFieldIndex = ".spec.controlPlaneRef.name"
)

var provisionerRateLimitBackoffAttempts sync.Map // map[string]int

// provisionerReservedSettings are the env names and flags that
// reconcileDeployment sets. Source (github.com/coder/coder/v2 v2.37.3, the
// version in go.mod): cli/root.go:67 and :99 (varURL "url", envURL
// "CODER_URL"), used at :418-419, and enterprise/cli/provisionerdaemonstart.go:285-286
// (--key, CODER_PROVISIONER_DAEMON_KEY).
var provisionerReservedSettings = reservedSettings{
	envNames: []string{"CODER_URL", "CODER_PROVISIONER_DAEMON_KEY"},
	flags:    []string{"url", "key"},
}

// CoderProvisionerReconciler reconciles a CoderProvisioner object.
type CoderProvisionerReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	BootstrapClient coderbootstrap.Client
}

// +kubebuilder:rbac:groups=coder.com,resources=coderprovisioners,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coder.com,resources=coderprovisioners/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coder.com,resources=coderprovisioners/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges the desired CoderProvisioner spec into Deployment, RBAC, and Secret resources.
func (r *CoderProvisionerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	if r.Client == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}
	if r.BootstrapClient == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: reconciler bootstrap client must not be nil")
	}

	provisioner := &coderv1alpha1.CoderProvisioner{}
	defer func() {
		switch {
		case err == nil:
			if result.RequeueAfter == 0 {
				resetProvisionerRateLimitBackoff(req.NamespacedName)
			}
			return
		case !coderbootstrap.IsRateLimitError(err):
			resetProvisionerRateLimitBackoff(req.NamespacedName)
			return
		}

		backoff := nextProvisionerRateLimitBackoff(req.NamespacedName)
		statusSnapshot := provisioner.Status.DeepCopy()
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
			metav1.ConditionFalse,
			"RateLimited",
			"Coder API rate limited while reconciling provisioner key",
		)
		if statusSnapshot != nil && !equality.Semantic.DeepEqual(*statusSnapshot, provisioner.Status) {
			if statusErr := r.Status().Update(ctx, provisioner); statusErr != nil && !apierrors.IsNotFound(statusErr) {
				ctrl.LoggerFrom(ctx).Error(statusErr, "failed to update coderprovisioner status after rate limit", "name", req.Name, "namespace", req.Namespace)
			}
		}

		ctrl.LoggerFrom(ctx).Info(
			"coder API rate limited during provisioner reconciliation; requeueing with backoff",
			"name", req.Name,
			"namespace", req.Namespace,
			"requeueAfter", backoff,
		)
		result = ctrl.Result{RequeueAfter: backoff}
		err = nil
	}()

	if err = r.Get(ctx, req.NamespacedName, provisioner); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get coderprovisioner %s: %w", req.NamespacedName, err)
	}

	if provisioner.Name != req.Name || provisioner.Namespace != req.Namespace {
		return ctrl.Result{}, fmt.Errorf("assertion failed: fetched object %s/%s does not match request %s/%s",
			provisioner.Namespace, provisioner.Name, req.Namespace, req.Name)
	}

	if !provisioner.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, provisioner)
	}

	statusSnapshot := provisioner.Status.DeepCopy()

	finalizerAdded, err := r.ensureCleanupFinalizer(ctx, provisioner)
	if err != nil {
		return ctrl.Result{}, err
	}
	if finalizerAdded {
		return ctrl.Result{}, nil
	}

	controlPlane, err := r.fetchControlPlane(ctx, provisioner)
	if err != nil {
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionControlPlaneReady,
			metav1.ConditionFalse,
			"ControlPlaneUnavailable",
			fmt.Sprintf("Failed to fetch control plane: %v", err),
		)
		_ = r.Status().Update(ctx, provisioner)
		return ctrl.Result{}, err
	}
	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionControlPlaneReady,
		metav1.ConditionTrue,
		"ControlPlaneAvailable",
		"Referenced control plane is available and has a URL",
	)

	// Fail static: a failed check sends nothing to Coder and leaves the
	// Deployment as it is. The Secret watch re-queues this provisioner.
	if err := r.checkProvisionerCredentials(ctx, provisioner, controlPlane); err != nil {
		credErr, ok := asCredentialError(err)
		if !ok {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.reportCredentialsNotReady(ctx, provisioner, statusSnapshot, credErr)
	}
	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionCredentialsReady,
		metav1.ConditionTrue,
		reasonCredentialsAccepted,
		credentialsAcceptedMessage,
	)

	organizationName := provisionerOrganizationName(provisioner.Spec.OrganizationName)
	keyName, keySecretName, keySecretKey := provisionerKeyConfig(provisioner)

	sessionToken, operatorAccessResult, operatorAccessReason, operatorAccessMessage, err := r.readOperatorSessionToken(ctx, provisioner, controlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}
	if operatorAccessResult.RequeueAfter > 0 {
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionOperatorAccessReady,
			metav1.ConditionFalse,
			operatorAccessReason,
			operatorAccessMessage,
		)
		_ = r.Status().Update(ctx, provisioner)
		return operatorAccessResult, nil
	}
	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionOperatorAccessReady,
		metav1.ConditionTrue,
		operatorAccessReason,
		operatorAccessMessage,
	)

	coderURL, err := coderAPIURL(controlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}

	entitlementResult, entitlementErr := r.reconcileExternalProvisionerEntitlement(ctx, provisioner, controlPlane, sessionToken)
	if entitlementErr != nil {
		if statusSnapshot == nil {
			return ctrl.Result{}, fmt.Errorf("assertion failed: status snapshot must not be nil")
		}
		if !equality.Semantic.DeepEqual(*statusSnapshot, provisioner.Status) {
			_ = r.Status().Update(ctx, provisioner)
		}
		return ctrl.Result{}, entitlementErr
	}
	if entitlementResult.RequeueAfter > 0 {
		if statusSnapshot == nil {
			return ctrl.Result{}, fmt.Errorf("assertion failed: status snapshot must not be nil")
		}
		if !equality.Semantic.DeepEqual(*statusSnapshot, provisioner.Status) {
			_ = r.Status().Update(ctx, provisioner)
		}
		return entitlementResult, nil
	}

	desiredTagsHash := hashProvisionerTags(provisioner.Spec.Tags)
	desiredControlPlaneRefName := provisioner.Spec.ControlPlaneRef.Name
	status := provisioner.Status
	orgDrift := status.OrganizationName != "" && status.OrganizationName != organizationName
	keyNameDrift := status.ProvisionerKeyName != "" && status.ProvisionerKeyName != keyName
	tagsDrift := status.TagsHash != "" && status.TagsHash != desiredTagsHash
	controlPlaneRefDrift := status.ControlPlaneRefName != "" && status.ControlPlaneRefName != desiredControlPlaneRefName
	controlPlaneURLDrift := status.ControlPlaneURL != "" && status.ControlPlaneURL != controlPlane.Status.URL
	driftDetected := orgDrift || keyNameDrift || tagsDrift || controlPlaneRefDrift || controlPlaneURLDrift
	appliedOrgName := provisioner.Status.OrganizationName
	appliedTagsHash := provisioner.Status.TagsHash
	appliedControlPlaneRefName := provisioner.Status.ControlPlaneRefName
	appliedControlPlaneURL := provisioner.Status.ControlPlaneURL

	// Check whether a usable provisioner key secret already exists.
	// The secret is considered "usable" only if this provisioner owns it
	// AND it contains a non-empty value at the configured data key.
	// checkProvisionerCredentials already refused an unowned Secret; read it
	// again here so a Secret replaced in between is refused too.
	existingSecret, err := r.getOwnedKeySecret(ctx, provisioner, keySecretName)
	if err != nil {
		if credErr, ok := asCredentialError(err); ok {
			return ctrl.Result{}, r.reportCredentialsNotReady(ctx, provisioner, statusSnapshot, credErr)
		}
		return ctrl.Result{}, err
	}
	secretUsable := existingSecret != nil && len(existingSecret.Data[keySecretKey]) > 0

	organizationID := provisioner.Status.OrganizationID
	provisionerKeyID := provisioner.Status.ProvisionerKeyID
	provisionerKeyName := provisioner.Status.ProvisionerKeyName
	if provisionerKeyName == "" {
		provisionerKeyName = keyName
	}

	log := ctrl.LoggerFrom(ctx)
	keyMaterial := ""
	if driftDetected {
		log.Info("spec drift detected, rotating provisioner key",
			"orgDrift", orgDrift, "keyNameDrift", keyNameDrift, "tagsDrift", tagsDrift,
			"controlPlaneRefDrift", controlPlaneRefDrift, "controlPlaneURLDrift", controlPlaneURLDrift)

		oldOrg := provisioner.Status.OrganizationName
		if oldOrg == "" {
			oldOrg = organizationName
		}
		oldKeyName := provisioner.Status.ProvisionerKeyName
		if oldKeyName == "" {
			oldKeyName = keyName
		}
		oldControlPlaneURL := recordedCoderAPIURL(provisioner.Status.ControlPlaneURL, controlPlane, coderURL)

		if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
			ctx,
			oldControlPlaneURL,
			sessionToken,
			oldOrg,
			oldKeyName,
		); deleteErr != nil {
			log.Info("failed to delete old provisioner key during drift rotation, creating new key anyway",
				"oldKeyName", oldKeyName, "error", deleteErr)
		}

		response, ensureErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
			CoderURL:         coderURL,
			SessionToken:     sessionToken,
			OrganizationName: organizationName,
			KeyName:          keyName,
			Tags:             provisioner.Spec.Tags,
		})
		if ensureErr != nil {
			setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
				metav1.ConditionFalse, "ProvisionerKeyFailed",
				fmt.Sprintf("Failed to ensure provisioner key %q after drift rotation", keyName))
			_ = r.Status().Update(ctx, provisioner)
			return ctrl.Result{}, fmt.Errorf("ensure provisioner key %q: %w", keyName, ensureErr)
		}
		if response.OrganizationID != uuid.Nil {
			organizationID = response.OrganizationID.String()
		}
		if response.KeyID != uuid.Nil {
			provisionerKeyID = response.KeyID.String()
		}
		if response.KeyName != "" {
			provisionerKeyName = response.KeyName
		}
		keyMaterial = response.Key
		if keyMaterial == "" {
			log.Info("drift-rotated key exists in coderd but returned no plaintext, rotating to recover",
				"keyName", keyName)

			if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
				ctx, coderURL, sessionToken, organizationName, keyName,
			); deleteErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to delete provisioner key %q for drift recovery", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("delete provisioner key %q for drift recovery: %w", keyName, deleteErr)
			}
			rotated, rotateErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL:         coderURL,
				SessionToken:     sessionToken,
				OrganizationName: organizationName,
				KeyName:          keyName,
				Tags:             provisioner.Spec.Tags,
			})
			if rotateErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to recreate provisioner key %q after drift recovery", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("recreate provisioner key %q after drift recovery: %w", keyName, rotateErr)
			}
			if rotated.OrganizationID != uuid.Nil {
				organizationID = rotated.OrganizationID.String()
			}
			if rotated.KeyID != uuid.Nil {
				provisionerKeyID = rotated.KeyID.String()
			}
			if rotated.KeyName != "" {
				provisionerKeyName = rotated.KeyName
			}
			keyMaterial = rotated.Key
			if keyMaterial == "" {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Provisioner key %q returned empty material after drift recovery rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("assertion failed: provisioner key %q returned empty material after drift recovery rotation", keyName)
			}
		}
		appliedOrgName = organizationName
		appliedTagsHash = desiredTagsHash
		appliedControlPlaneRefName = desiredControlPlaneRefName
		appliedControlPlaneURL = controlPlane.Status.URL
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
			metav1.ConditionTrue,
			"ProvisionerKeyReady",
			"Provisioner key is available in coderd",
		)
	} else if !secretUsable {
		response, ensureErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
			CoderURL:         coderURL,
			SessionToken:     sessionToken,
			OrganizationName: organizationName,
			KeyName:          keyName,
			Tags:             provisioner.Spec.Tags,
		})
		if ensureErr != nil {
			setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
				metav1.ConditionFalse, "ProvisionerKeyFailed",
				fmt.Sprintf("Failed to ensure provisioner key %q", keyName))
			_ = r.Status().Update(ctx, provisioner)
			return ctrl.Result{}, fmt.Errorf("ensure provisioner key %q: %w", keyName, ensureErr)
		}
		if response.OrganizationID != uuid.Nil {
			organizationID = response.OrganizationID.String()
		}
		if response.KeyID != uuid.Nil {
			provisionerKeyID = response.KeyID.String()
		}
		if response.KeyName != "" {
			provisionerKeyName = response.KeyName
		}
		keyMaterial = response.Key

		// If the key already exists in coderd (e.g. the K8s secret was
		// deleted), coderd won't return plaintext again. Rotate the key
		// by deleting and recreating it to obtain fresh material.
		if keyMaterial == "" {
			log.Info("provisioner key exists in coderd but secret is missing, rotating key to recover",
				"keyName", keyName, "secretName", keySecretName)

			if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
				ctx, coderURL, sessionToken, organizationName, keyName,
			); deleteErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to delete stale provisioner key %q for rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("delete stale provisioner key %q for rotation: %w", keyName, deleteErr)
			}
			rotated, rotateErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL:         coderURL,
				SessionToken:     sessionToken,
				OrganizationName: organizationName,
				KeyName:          keyName,
				Tags:             provisioner.Spec.Tags,
			})
			if rotateErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to recreate provisioner key %q after rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("recreate provisioner key %q after rotation: %w", keyName, rotateErr)
			}
			if rotated.OrganizationID != uuid.Nil {
				organizationID = rotated.OrganizationID.String()
			}
			if rotated.KeyID != uuid.Nil {
				provisionerKeyID = rotated.KeyID.String()
			}
			if rotated.KeyName != "" {
				provisionerKeyName = rotated.KeyName
			}
			keyMaterial = rotated.Key
			if keyMaterial == "" {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Provisioner key %q returned empty key material after rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("assertion failed: provisioner key %q returned empty key material after rotation", keyName)
			}
		}
		appliedOrgName = organizationName
		appliedTagsHash = desiredTagsHash
		appliedControlPlaneRefName = desiredControlPlaneRefName
		appliedControlPlaneURL = controlPlane.Status.URL
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
			metav1.ConditionTrue,
			"ProvisionerKeyReady",
			"Provisioner key is available in coderd",
		)
	} else if status.OrganizationName == "" || status.TagsHash == "" || status.ControlPlaneRefName == "" || status.ControlPlaneURL == "" {
		// Secret is usable and no drift detected, but status metadata is empty
		// (e.g. upgrade from older version). Call EnsureProvisionerKey to populate
		// IDs and key name. If coderd reports an existing key (no plaintext key
		// returned), rotate it best-effort so desired tags are applied before
		// stamping the metadata baseline.
		response, ensureErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
			CoderURL:         coderURL,
			SessionToken:     sessionToken,
			OrganizationName: organizationName,
			KeyName:          keyName,
			Tags:             provisioner.Spec.Tags,
		})
		if ensureErr != nil {
			// Return a requeue so metadata population retries.
			return ctrl.Result{}, fmt.Errorf("verify provisioner key metadata: %w", ensureErr)
		}
		if response.OrganizationID != uuid.Nil {
			organizationID = response.OrganizationID.String()
		}
		if response.KeyID != uuid.Nil {
			provisionerKeyID = response.KeyID.String()
		}
		if response.KeyName != "" {
			provisionerKeyName = response.KeyName
		}
		if response.Key != "" {
			// Key was freshly created with desired tags; capture material and stamp baseline.
			keyMaterial = response.Key
			appliedOrgName = organizationName
			appliedTagsHash = desiredTagsHash
			appliedControlPlaneRefName = desiredControlPlaneRefName
			appliedControlPlaneURL = controlPlane.Status.URL
		} else {
			// Key already exists; tags may be stale. Rotate to ensure desired tags are applied.
			log.Info("existing key found during metadata backfill, rotating to ensure desired tags",
				"keyName", keyName)
			if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
				ctx, coderURL, sessionToken, organizationName, keyName,
			); deleteErr != nil {
				return ctrl.Result{}, fmt.Errorf("delete provisioner key %q for metadata backfill rotation: %w", keyName, deleteErr)
			}
			rotated, rotateErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL:         coderURL,
				SessionToken:     sessionToken,
				OrganizationName: organizationName,
				KeyName:          keyName,
				Tags:             provisioner.Spec.Tags,
			})
			if rotateErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to recreate provisioner key %q after metadata backfill rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("recreate provisioner key %q after metadata backfill rotation: %w", keyName, rotateErr)
			}
			if rotated.OrganizationID != uuid.Nil {
				organizationID = rotated.OrganizationID.String()
			}
			if rotated.KeyID != uuid.Nil {
				provisionerKeyID = rotated.KeyID.String()
			}
			if rotated.KeyName != "" {
				provisionerKeyName = rotated.KeyName
			}
			keyMaterial = rotated.Key
			if keyMaterial == "" {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Provisioner key %q returned empty material after metadata backfill rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("assertion failed: provisioner key %q returned empty material after metadata backfill rotation", keyName)
			}
			appliedOrgName = organizationName
			appliedTagsHash = desiredTagsHash
			appliedControlPlaneRefName = desiredControlPlaneRefName
			appliedControlPlaneURL = controlPlane.Status.URL
		}
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
			metav1.ConditionTrue,
			"ProvisionerKeyReady",
			"Provisioner key is available in coderd",
		)
	}

	if keyMaterial != "" {
		// A new key exists in Coder. Record its IDs in status before its
		// plaintext reaches the Secret, so that deletion can find the key to
		// revoke (at status.controlPlaneURL or, during a control plane URL
		// change, at the current control plane URL). Only the IDs: the applied spec values stay as they are
		// until the Secret holds the key, so a failed Secret write is detected
		// as drift (or missing metadata) and rotated again on the next try.
		// A failed status write (for example a conflict because the
		// provisioner is being deleted) revokes the new key before anyone can
		// read it.
		provisioner.Status.OrganizationID = organizationID
		provisioner.Status.ProvisionerKeyID = provisionerKeyID
		if err := r.Status().Update(ctx, provisioner); err != nil {
			r.revokeUnpublishedKey(ctx, coderURL, sessionToken, organizationID, provisionerKeyID)
			return ctrl.Result{}, fmt.Errorf("record new provisioner key in status: %w", err)
		}
		statusSnapshot = provisioner.Status.DeepCopy()
		if statusSnapshot == nil {
			return ctrl.Result{}, fmt.Errorf("assertion failed: status snapshot must not be nil after new key status update")
		}
	}

	provisionerKeySecret, err := r.ensureProvisionerKeySecret(ctx, provisioner, keySecretName, keySecretKey, keyMaterial)
	if err != nil {
		if credErr, ok := asCredentialError(err); ok {
			return ctrl.Result{}, r.reportCredentialsNotReady(ctx, provisioner, statusSnapshot, credErr)
		}
		return ctrl.Result{}, err
	}

	secretValue, ok := provisionerKeySecret.Data[keySecretKey]
	if !ok {
		return ctrl.Result{}, fmt.Errorf("assertion failed: provisioner key secret %q is missing key %q after reconciliation", keySecretName, keySecretKey)
	}
	if len(secretValue) == 0 {
		return ctrl.Result{}, fmt.Errorf("assertion failed: provisioner key secret %q key %q is empty after reconciliation", keySecretName, keySecretKey)
	}
	secretChecksum := hashProvisionerSecret(secretValue)

	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionProvisionerKeySecretReady,
		metav1.ConditionTrue,
		"SecretReady",
		"Provisioner key secret is available",
	)

	// Persist key-related status metadata immediately so retries after later
	// reconciliation failures (for example RBAC/deployment errors) do not
	// repeatedly re-run metadata backfill/rotation against coderd.
	keyStatusSnapshot := provisioner.Status.DeepCopy()
	if keyStatusSnapshot == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: key status snapshot must not be nil")
	}
	recordAppliedKeyStatus(provisioner, organizationID, appliedOrgName, provisionerKeyID, provisionerKeyName,
		appliedTagsHash, appliedControlPlaneRefName, appliedControlPlaneURL)
	provisioner.Status.SecretRef = &coderv1alpha1.SecretKeySelector{Name: keySecretName, Key: keySecretKey}
	if !equality.Semantic.DeepEqual(*keyStatusSnapshot, provisioner.Status) {
		if err := r.Status().Update(ctx, provisioner); err != nil {
			return ctrl.Result{}, fmt.Errorf("update coderprovisioner key status: %w", err)
		}
		statusSnapshot = provisioner.Status.DeepCopy()
		if statusSnapshot == nil {
			return ctrl.Result{}, fmt.Errorf("assertion failed: status snapshot must not be nil after key status update")
		}
	}
	serviceAccountName := provisionerServiceAccountName(provisioner.Name)
	if _, err := r.reconcileServiceAccount(ctx, provisioner, serviceAccountName); err != nil {
		return ctrl.Result{}, err
	}

	roleName := provisionerResourceName(provisioner.Name)
	role, err := r.reconcileRole(ctx, provisioner, roleName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.reconcileRoleBinding(ctx, provisioner, roleName, role.Name, serviceAccountName); err != nil {
		return ctrl.Result{}, err
	}

	image := provisioner.Spec.Image
	if image == "" {
		image = controlPlane.Spec.Image
	}
	if image == "" {
		image = defaultCoderImage
	}

	secretRef := &coderv1alpha1.SecretKeySelector{Name: keySecretName, Key: keySecretKey}
	deployment, err := r.reconcileDeployment(ctx, provisioner, image, controlPlane.Status.URL, secretRef, serviceAccountName, secretChecksum)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(
		ctx,
		provisioner,
		deployment,
		secretRef,
		organizationID,
		appliedOrgName,
		provisionerKeyID,
		provisionerKeyName,
		appliedTagsHash,
		appliedControlPlaneRefName,
		appliedControlPlaneURL,
		statusSnapshot,
	); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CoderProvisionerReconciler) reconcileDeletion(ctx context.Context, provisioner *coderv1alpha1.CoderProvisioner) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(provisioner, coderv1alpha1.ProvisionerKeyCleanupFinalizer) {
		return ctrl.Result{}, nil
	}

	log := ctrl.LoggerFrom(ctx)

	// Delete only the key that status records for this resource. A provisioner
	// that never applied a key (refused, not entitled, never ready) must not
	// delete a Coder key that happens to have the name from its spec.
	organizationID, keyID, recorded := recordedProvisionerKey(provisioner.Status)
	if !recorded {
		log.Info("status records no Coder provisioner key, removing the finalizer without a Coder call")
		return r.removeCleanupFinalizer(ctx, provisioner)
	}
	keyName := provisioner.Status.ProvisionerKeyName

	// Best-effort remote key cleanup: if the referenced control plane,
	// its URL, operator access token, or any other prerequisite is
	// unavailable, log a warning and proceed to finalizer removal so the
	// CR does not get stuck in Terminating. This is common during
	// namespace teardown, when the control plane was never ready, or
	// when operator access bootstrap has not completed.
	controlPlaneURL := provisioner.Status.ControlPlaneURL
	controlPlaneName := strings.TrimSpace(provisioner.Spec.ControlPlaneRef.Name)
	var controlPlane *coderv1alpha1.CoderControlPlane
	if controlPlaneName != "" {
		controlPlane = &coderv1alpha1.CoderControlPlane{}
		namespacedName := types.NamespacedName{Name: controlPlaneName, Namespace: provisioner.Namespace}
		if err := r.Get(ctx, namespacedName, controlPlane); err != nil {
			log.Info("unable to read referenced CoderControlPlane during deletion, skipping remote key cleanup",
				"controlPlaneRef", controlPlaneName, "error", err)
			controlPlane = nil
		} else if controlPlane.Name != controlPlaneName || controlPlane.Namespace != provisioner.Namespace {
			return ctrl.Result{}, fmt.Errorf("assertion failed: fetched control plane %s/%s does not match expected %s/%s",
				controlPlane.Namespace, controlPlane.Name, provisioner.Namespace, controlPlaneName)
		}
	}
	if controlPlaneURL == "" && controlPlane != nil {
		controlPlaneURL = strings.TrimSpace(controlPlane.Status.URL)
	}

	if controlPlaneURL != "" && controlPlane != nil {
		sessionToken, tokenResult, tokenReason, tokenMessage, tokenErr := r.readOperatorSessionToken(ctx, provisioner, controlPlane)
		switch {
		case tokenErr != nil:
			log.Info("unable to resolve operator access token during deletion, skipping remote key cleanup",
				"error", tokenErr)
		case tokenResult.RequeueAfter > 0:
			log.Info("operator access is not ready during deletion, skipping remote key cleanup",
				"reason", tokenReason, "message", tokenMessage)
		default:
			currentURL, urlErr := coderAPIURL(controlPlane)
			if urlErr != nil {
				log.Info("unable to derive the Coder API URL during deletion, skipping remote key cleanup", "error", urlErr)
				break
			}
			// During a control plane URL change, status can hold the new key's
			// IDs while status.controlPlaneURL still names the old endpoint.
			// Try the current endpoint too: the delete matches the key ID only.
			coderURLs := []string{recordedCoderAPIURL(provisioner.Status.ControlPlaneURL, controlPlane, currentURL)}
			if currentURL != coderURLs[0] {
				coderURLs = append(coderURLs, currentURL)
			}
			for _, coderURL := range coderURLs {
				if deleteErr := r.BootstrapClient.DeleteProvisionerKeyByID(ctx, coderbootstrap.DeleteProvisionerKeyByIDRequest{
					CoderURL:       coderURL,
					SessionToken:   sessionToken,
					OrganizationID: organizationID,
					KeyID:          keyID,
				}); deleteErr != nil {
					// Treat key deletion failures as best-effort so the
					// finalizer is still removed. Transient errors, auth
					// issues, or org-lookup failures should not block CR
					// cleanup.
					log.Info("failed to delete remote provisioner key during deletion, proceeding with finalizer removal",
						"keyName", keyName, "keyID", keyID, "error", deleteErr)
				}
			}
		}
	}

	return r.removeCleanupFinalizer(ctx, provisioner)
}

// coderAPIURL returns the URL for Coder API calls with the operator token.
// With TLS it is the internal HTTP URL, not status.url (see
// controlplaneurl.CoderAPIURL). The provisioner Deployment and the recorded
// status.controlPlaneURL still use status.url.
func coderAPIURL(controlPlane *coderv1alpha1.CoderControlPlane) (string, error) {
	coderURL, err := controlplaneurl.CoderAPIURL(controlPlane)
	if err != nil {
		return "", fmt.Errorf("derive the Coder API URL of CoderControlPlane %s/%s: %w", controlPlane.Namespace, controlPlane.Name, err)
	}
	return coderURL.String(), nil
}

// recordedCoderAPIURL maps a status.url that status.controlPlaneURL recorded
// to the URL for Coder API calls. An empty recorded URL, or one equal to the
// control plane's current status.url, maps to currentAPIURL. Any other URL
// names an earlier endpoint and is used as recorded.
func recordedCoderAPIURL(recordedURL string, controlPlane *coderv1alpha1.CoderControlPlane, currentAPIURL string) string {
	recordedURL = strings.TrimSpace(recordedURL)
	if recordedURL == "" || recordedURL == strings.TrimSpace(controlPlane.Status.URL) {
		return currentAPIURL
	}
	return recordedURL
}

// recordAppliedKeyStatus sets the status fields that describe the key applied
// in Coder.
func recordAppliedKeyStatus(
	provisioner *coderv1alpha1.CoderProvisioner,
	organizationID, organizationName, keyID, keyName, tagsHash, controlPlaneRefName, controlPlaneURL string,
) {
	provisioner.Status.OrganizationID = organizationID
	provisioner.Status.OrganizationName = organizationName
	provisioner.Status.ProvisionerKeyID = keyID
	provisioner.Status.ProvisionerKeyName = keyName
	provisioner.Status.TagsHash = tagsHash
	provisioner.Status.ControlPlaneRefName = controlPlaneRefName
	provisioner.Status.ControlPlaneURL = controlPlaneURL
}

// revokeUnpublishedKey deletes a key whose plaintext was never written to a
// Secret. It is best-effort: the key is unknown outside this reconcile.
func (r *CoderProvisionerReconciler) revokeUnpublishedKey(ctx context.Context, coderURL, sessionToken, organizationID, keyID string) {
	log := ctrl.LoggerFrom(ctx)
	orgID, orgErr := uuid.Parse(organizationID)
	parsedKeyID, keyErr := uuid.Parse(keyID)
	if orgErr != nil || keyErr != nil || orgID == uuid.Nil || parsedKeyID == uuid.Nil {
		log.Info("cannot revoke the unpublished provisioner key: Coder returned no key ID")
		return
	}
	if err := r.BootstrapClient.DeleteProvisionerKeyByID(ctx, coderbootstrap.DeleteProvisionerKeyByIDRequest{
		CoderURL:       coderURL,
		SessionToken:   sessionToken,
		OrganizationID: orgID,
		KeyID:          parsedKeyID,
	}); err != nil {
		log.Info("failed to revoke the unpublished provisioner key", "keyID", parsedKeyID, "error", err)
	}
}

func (r *CoderProvisionerReconciler) removeCleanupFinalizer(ctx context.Context, provisioner *coderv1alpha1.CoderProvisioner) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(provisioner, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
	if err := r.Update(ctx, provisioner); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from coderprovisioner %s/%s: %w", provisioner.Namespace, provisioner.Name, err)
	}

	return ctrl.Result{}, nil
}

// recordedProvisionerKey returns the organization and key IDs that status
// records for the key this provisioner created. ok is false when status does
// not record valid IDs.
func recordedProvisionerKey(status coderv1alpha1.CoderProvisionerStatus) (organizationID, keyID uuid.UUID, ok bool) {
	organizationID, err := uuid.Parse(status.OrganizationID)
	if err != nil || organizationID == uuid.Nil {
		return uuid.Nil, uuid.Nil, false
	}
	keyID, err = uuid.Parse(status.ProvisionerKeyID)
	if err != nil || keyID == uuid.Nil {
		return uuid.Nil, uuid.Nil, false
	}

	return organizationID, keyID, true
}

func (r *CoderProvisionerReconciler) ensureCleanupFinalizer(ctx context.Context, provisioner *coderv1alpha1.CoderProvisioner) (bool, error) {
	if controllerutil.ContainsFinalizer(provisioner, coderv1alpha1.ProvisionerKeyCleanupFinalizer) {
		return false, nil
	}

	controllerutil.AddFinalizer(provisioner, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
	if err := r.Update(ctx, provisioner); err != nil {
		return false, fmt.Errorf("add finalizer to coderprovisioner %s/%s: %w", provisioner.Namespace, provisioner.Name, err)
	}

	return true, nil
}

func (r *CoderProvisionerReconciler) fetchControlPlane(ctx context.Context, provisioner *coderv1alpha1.CoderProvisioner) (*coderv1alpha1.CoderControlPlane, error) {
	controlPlaneName := provisioner.Spec.ControlPlaneRef.Name
	if controlPlaneName == "" {
		return nil, fmt.Errorf("coderprovisioner %s/%s spec.controlPlaneRef.name is required", provisioner.Namespace, provisioner.Name)
	}

	controlPlane := &coderv1alpha1.CoderControlPlane{}
	namespacedName := types.NamespacedName{Name: controlPlaneName, Namespace: provisioner.Namespace}
	if err := r.Get(ctx, namespacedName, controlPlane); err != nil {
		return nil, fmt.Errorf("get referenced codercontrolplane %s for coderprovisioner %s/%s: %w", namespacedName, provisioner.Namespace, provisioner.Name, err)
	}

	if controlPlane.Name != controlPlaneName || controlPlane.Namespace != provisioner.Namespace {
		return nil, fmt.Errorf("assertion failed: fetched control plane %s/%s does not match expected %s/%s",
			controlPlane.Namespace, controlPlane.Name, provisioner.Namespace, controlPlaneName)
	}
	if controlPlane.Status.URL == "" {
		return nil, fmt.Errorf("codercontrolplane %s/%s status.url is empty", controlPlane.Namespace, controlPlane.Name)
	}

	return controlPlane, nil
}

func (r *CoderProvisionerReconciler) readOperatorSessionToken(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	controlPlane *coderv1alpha1.CoderControlPlane,
) (string, ctrl.Result, string, string, error) {
	if provisioner == nil {
		return "", ctrl.Result{}, "", "", fmt.Errorf("assertion failed: coder provisioner must not be nil")
	}
	if controlPlane == nil {
		return "", ctrl.Result{}, "", "", fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	if !controlPlane.Status.OperatorAccessReady {
		return "", ctrl.Result{RequeueAfter: provisionerOperatorAccessRetryInterval},
			"OperatorAccessNotReady",
			fmt.Sprintf("Waiting for CoderControlPlane %s/%s status.operatorAccessReady=true before reconciling provisioner keys.", controlPlane.Namespace, controlPlane.Name),
			nil
	}

	operatorTokenRef := controlPlane.Status.OperatorTokenSecretRef
	if operatorTokenRef == nil {
		return "", ctrl.Result{RequeueAfter: provisionerOperatorAccessRetryInterval},
			"OperatorTokenSecretRefMissing",
			fmt.Sprintf("Waiting for CoderControlPlane %s/%s status.operatorTokenSecretRef to be set by operator access bootstrap.", controlPlane.Namespace, controlPlane.Name),
			nil
	}

	operatorTokenSecretName := strings.TrimSpace(operatorTokenRef.Name)
	if operatorTokenSecretName == "" {
		return "", ctrl.Result{RequeueAfter: provisionerOperatorAccessRetryInterval},
			"OperatorTokenSecretRefInvalid",
			fmt.Sprintf("Waiting for CoderControlPlane %s/%s status.operatorTokenSecretRef.name to be non-empty.", controlPlane.Namespace, controlPlane.Name),
			nil
	}
	operatorTokenSecretKey := strings.TrimSpace(operatorTokenRef.Key)
	if operatorTokenSecretKey == "" {
		operatorTokenSecretKey = coderv1alpha1.DefaultTokenSecretKey
	}

	token, err := operatortoken.Read(ctx, r.Client, controlPlane)
	if errors.Is(err, operatortoken.ErrUnavailable) {
		return "", ctrl.Result{RequeueAfter: provisionerOperatorAccessRetryInterval},
			"OperatorTokenSecretUnavailable",
			fmt.Sprintf("Failed to read operator token Secret %q/%q key %q from referenced control plane status: %v", provisioner.Namespace, operatorTokenSecretName, operatorTokenSecretKey, err),
			nil
	} else if err != nil {
		return "", ctrl.Result{}, "", "", err
	}

	return token, ctrl.Result{}, "OperatorAccessReady", "Operator-managed access token from referenced control plane is available", nil
}

// checkProvisionerCredentials rejects reserved extraEnv and extraArgs entries,
// refuses a provisioner key Secret that this provisioner does not own, and
// applies the credential Secret contract to every extraEnv secretKeyRef.
// The destination is the CODER_URL that reconcileDeployment sets.
func (r *CoderProvisionerReconciler) checkProvisionerCredentials(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	controlPlane *coderv1alpha1.CoderControlPlane,
) error {
	if provisioner == nil {
		return fmt.Errorf("assertion failed: coder provisioner must not be nil")
	}
	if controlPlane == nil {
		return fmt.Errorf("assertion failed: coder control plane must not be nil")
	}

	if err := checkReservedSettings(provisioner.Spec.ExtraEnv, provisioner.Spec.ExtraArgs, provisionerReservedSettings); err != nil {
		return err
	}
	_, keySecretName, _ := provisionerKeyConfig(provisioner)
	if _, err := r.getOwnedKeySecret(ctx, provisioner, keySecretName); err != nil {
		return err
	}
	usesSecrets := slices.ContainsFunc(provisioner.Spec.ExtraEnv, func(env corev1.EnvVar) bool {
		return env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil
	})
	if !usesSecrets {
		return nil
	}
	coderURL, err := credentialsecret.ParseURL(controlPlane.Status.URL)
	if err != nil {
		return fmt.Errorf("codercontrolplane %s/%s status.url: %w", controlPlane.Namespace, controlPlane.Name, err)
	}
	return checkExtraEnvSecrets(ctx, r.Client, provisioner.Namespace, provisioner.Spec.ExtraEnv, coderURL)
}

func (r *CoderProvisionerReconciler) reconcileExternalProvisionerEntitlement(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	controlPlane *coderv1alpha1.CoderControlPlane,
	sessionToken string,
) (ctrl.Result, error) {
	if provisioner == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder provisioner must not be nil")
	}
	if controlPlane == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder control plane must not be nil")
	}
	if strings.TrimSpace(controlPlane.Status.URL) == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: coder control plane URL must not be empty")
	}
	if sessionToken == "" {
		return ctrl.Result{}, fmt.Errorf("assertion failed: operator session token must not be empty")
	}

	if controlPlane.Status.EntitlementsLastChecked != nil {
		recheckAfter := durationUntilNextEntitlementsCheck(controlPlane.Status.EntitlementsLastChecked, externalProvisionerEntitlementRetryInterval)

		switch strings.TrimSpace(controlPlane.Status.ExternalProvisionerDaemonsEntitlement) {
		case string(codersdk.EntitlementEntitled), string(codersdk.EntitlementGracePeriod):
			if recheckAfter > 0 {
				setCondition(
					provisioner,
					coderv1alpha1.CoderProvisionerConditionExternalProvisionersEntitled,
					metav1.ConditionTrue,
					"Entitled",
					"Coder deployment is entitled to external provisioner daemons",
				)
				return ctrl.Result{}, nil
			}
		case string(codersdk.EntitlementNotEntitled):
			if recheckAfter > 0 {
				setCondition(
					provisioner,
					coderv1alpha1.CoderProvisionerConditionExternalProvisionersEntitled,
					metav1.ConditionFalse,
					"NotEntitled",
					"Coder deployment is not entitled to external provisioner daemons; install a Premium/Enterprise license to enable external provisioners.",
				)
				return ctrl.Result{RequeueAfter: recheckAfter}, nil
			}
		}
	}

	coderURL, err := coderAPIURL(controlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}
	entitlements, err := r.BootstrapClient.Entitlements(ctx, coderURL, sessionToken)
	if err != nil {
		reason := "EntitlementsQueryFailed"
		message := "Failed to query Coder entitlements; retrying."

		var apiErr *codersdk.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode() {
			case http.StatusNotFound:
				reason = "NotSupported"
				message = "Coder deployment does not expose /api/v2/entitlements; cannot verify license."
			case http.StatusUnauthorized, http.StatusForbidden:
				reason = "Forbidden"
				message = "Operator access token is not authorized to read entitlements; retrying."
			}
		}

		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionExternalProvisionersEntitled,
			metav1.ConditionFalse,
			reason,
			message,
		)
		return ctrl.Result{RequeueAfter: externalProvisionerEntitlementRetryInterval}, nil
	}
	if entitlements.Features == nil {
		return ctrl.Result{}, fmt.Errorf("assertion failed: entitlements features must not be nil")
	}

	feature, ok := entitlements.Features[codersdk.FeatureExternalProvisionerDaemons]
	if !ok || !feature.Entitlement.Entitled() {
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionExternalProvisionersEntitled,
			metav1.ConditionFalse,
			"NotEntitled",
			"Coder deployment is not entitled to external provisioner daemons; install a Premium/Enterprise license to enable external provisioners.",
		)
		return ctrl.Result{RequeueAfter: externalProvisionerEntitlementRetryInterval}, nil
	}

	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionExternalProvisionersEntitled,
		metav1.ConditionTrue,
		"Entitled",
		"Coder deployment is entitled to external provisioner daemons",
	)
	return ctrl.Result{}, nil
}

// ensureProvisionerKeySecret creates the provisioner key Secret when it is
// absent, or updates it when this provisioner controls it. It never adopts or
// writes a Secret that this provisioner does not control: that gives a
// KeySecretNotOwned error.
func (r *CoderProvisionerReconciler) ensureProvisionerKeySecret(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	secretName string,
	secretKey string,
	keyMaterial string,
) (*corev1.Secret, error) {
	existing, err := r.getOwnedKeySecret(ctx, provisioner, secretName)
	if err != nil {
		return nil, err
	}
	labels := provisionerLabels(provisioner.Name)

	if existing == nil {
		if keyMaterial == "" {
			return nil, fmt.Errorf("provisioner key secret %q key %q is empty", secretName, secretKey)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: provisioner.Namespace, Labels: maps.Clone(labels)},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{secretKey: []byte(keyMaterial)},
		}
		if err := controllerutil.SetControllerReference(provisioner, secret, r.Scheme); err != nil {
			return nil, fmt.Errorf("set controller reference on provisioner key secret %q: %w", secretName, err)
		}
		if err := r.Create(ctx, secret); err != nil {
			return nil, fmt.Errorf("create provisioner key secret %q: %w", secretName, err)
		}
		return secret, nil
	}

	if !isControlledBy(existing, provisioner, "CoderProvisioner") {
		return nil, fmt.Errorf("assertion failed: provisioner key secret %q must be controlled by this provisioner before an update", secretName)
	}
	updated := existing.DeepCopy()
	updated.Labels = maps.Clone(labels)
	if updated.Data == nil {
		updated.Data = make(map[string][]byte)
	}
	if keyMaterial != "" {
		updated.Data[secretKey] = []byte(keyMaterial)
	}
	if len(updated.Data[secretKey]) == 0 {
		return nil, fmt.Errorf("provisioner key secret %q key %q is empty", secretName, secretKey)
	}
	if equality.Semantic.DeepEqual(existing, updated) {
		return existing, nil
	}
	if err := r.Update(ctx, updated); err != nil {
		return nil, fmt.Errorf("update provisioner key secret %q: %w", secretName, err)
	}
	return updated, nil
}

// getOwnedKeySecret returns the provisioner key Secret, or nil when it does
// not exist. It returns a KeySecretNotOwned error when the Secret exists and
// this provisioner is not its controller: the operator then does not read,
// mount, write or adopt it.
func (r *CoderProvisionerReconciler) getOwnedKeySecret(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	name string,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: provisioner.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get provisioner key secret %q: %w", name, err)
	}
	if isControlledBy(secret, provisioner, "CoderProvisioner") {
		return secret, nil
	}

	message := fmt.Sprintf("Secret %q named in spec.key.secretName exists and is not owned by this provisioner. "+
		"The operator does not read or change it. Delete that Secret so the operator creates a new provisioner key, "+
		"or set spec.key.secretName to a name that does not exist yet.", name)
	if strings.TrimSpace(provisioner.Spec.Key.SecretName) == "" {
		message = fmt.Sprintf("Secret %q (the default provisioner key Secret) exists and is not owned by this provisioner. "+
			"The operator does not read or change it. Delete that Secret so the operator creates a new provisioner key, "+
			"or set spec.key.secretName to a name that does not exist yet.", name)
	}
	return nil, &credentialsecret.Error{Reason: reasonKeySecretNotOwned, Message: message}
}

// reportCredentialsNotReady records a failed check in CredentialsReady and
// changes nothing else.
func (r *CoderProvisionerReconciler) reportCredentialsNotReady(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	statusSnapshot *coderv1alpha1.CoderProvisionerStatus,
	credErr *credentialsecret.Error,
) error {
	if credErr == nil {
		return fmt.Errorf("assertion failed: credential error must not be nil")
	}
	if statusSnapshot == nil {
		return fmt.Errorf("assertion failed: status snapshot must not be nil")
	}
	ctrl.LoggerFrom(ctx).Info("provisioner credential check failed; keeping the last applied configuration",
		"reason", credErr.Reason, "message", credErr.Message)
	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionCredentialsReady,
		metav1.ConditionFalse,
		credErr.Reason,
		credErr.Message+" The provisioner keeps its last applied configuration.",
	)
	if equality.Semantic.DeepEqual(*statusSnapshot, provisioner.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, provisioner); err != nil {
		return fmt.Errorf("update coderprovisioner credentials condition: %w", err)
	}
	return nil
}

func (r *CoderProvisionerReconciler) reconcileServiceAccount(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	serviceAccountName string,
) (*corev1.ServiceAccount, error) {
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: provisioner.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		labels := provisionerLabels(provisioner.Name)
		serviceAccount.Labels = maps.Clone(labels)
		if err := controllerutil.SetControllerReference(provisioner, serviceAccount, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile serviceaccount %q: %w", serviceAccountName, err)
	}

	return serviceAccount, nil
}

func (r *CoderProvisionerReconciler) reconcileRole(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	roleName string,
) (*rbacv1.Role, error) {
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: provisioner.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		labels := provisionerLabels(provisioner.Name)
		role.Labels = maps.Clone(labels)
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"pods", "persistentvolumeclaims"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}}
		if err := controllerutil.SetControllerReference(provisioner, role, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile role %q: %w", roleName, err)
	}

	return role, nil
}

func (r *CoderProvisionerReconciler) reconcileRoleBinding(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	roleBindingName string,
	roleName string,
	serviceAccountName string,
) (*rbacv1.RoleBinding, error) {
	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleBindingName, Namespace: provisioner.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
		labels := provisionerLabels(provisioner.Name)
		roleBinding.Labels = maps.Clone(labels)
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		}
		roleBinding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      serviceAccountName,
			Namespace: provisioner.Namespace,
		}}
		if err := controllerutil.SetControllerReference(provisioner, roleBinding, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile rolebinding %q: %w", roleBindingName, err)
	}

	return roleBinding, nil
}

func (r *CoderProvisionerReconciler) reconcileDeployment(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	image string,
	coderURL string,
	secretRef *coderv1alpha1.SecretKeySelector,
	serviceAccountName string,
	secretChecksum string,
) (*appsv1.Deployment, error) {
	deploymentName := provisionerResourceName(provisioner.Name)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: provisioner.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := provisionerLabels(provisioner.Name)
		deployment.Labels = maps.Clone(labels)

		if err := controllerutil.SetControllerReference(provisioner, deployment, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		replicas := defaultProvisionerReplicas
		if provisioner.Spec.Replicas != nil {
			replicas = *provisioner.Spec.Replicas
		}
		terminationGracePeriodSeconds := defaultProvisionerTerminationGracePeriodSeconds
		if provisioner.Spec.TerminationGracePeriodSeconds != nil {
			terminationGracePeriodSeconds = *provisioner.Spec.TerminationGracePeriodSeconds
		}

		args := []string{"provisioner", "start"}
		args = append(args, provisioner.Spec.ExtraArgs...)

		env := []corev1.EnvVar{
			{Name: "CODER_URL", Value: coderURL},
			{
				Name: "CODER_PROVISIONER_DAEMON_KEY",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretRef.Name},
					Key:                  secretRef.Key,
				}},
			},
		}
		env = append(env, provisioner.Spec.ExtraEnv...)

		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}
		deployment.Spec.Template = withPreservedRolloutRestart(deployment.Spec.Template, corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: maps.Clone(labels),
				Annotations: map[string]string{
					provisionerKeyChecksumAnnotation: secretChecksum,
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName:            serviceAccountName,
				ImagePullSecrets:              provisioner.Spec.ImagePullSecrets,
				TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
				Containers: []corev1.Container{{
					Name:      "provisioner",
					Image:     image,
					Command:   []string{"coder"},
					Args:      args,
					Env:       env,
					Resources: provisioner.Spec.Resources,
				}},
			},
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile provisioner deployment: %w", err)
	}

	return deployment, nil
}

func (r *CoderProvisionerReconciler) reconcileStatus(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	deployment *appsv1.Deployment,
	secretRef *coderv1alpha1.SecretKeySelector,
	organizationID string,
	organizationName string,
	provisionerKeyID string,
	provisionerKeyName string,
	tagsHash string,
	controlPlaneRefName string,
	controlPlaneURL string,
	statusSnapshot *coderv1alpha1.CoderProvisionerStatus,
) error {
	if statusSnapshot == nil {
		return fmt.Errorf("assertion failed: status snapshot must not be nil")
	}

	phase := coderv1alpha1.CoderProvisionerPhasePending
	if deployment.Status.ReadyReplicas > 0 {
		phase = coderv1alpha1.CoderProvisionerPhaseReady
	}

	// Update fields individually so conditions set earlier in reconciliation are preserved.
	provisioner.Status.ObservedGeneration = provisioner.Generation
	provisioner.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	provisioner.Status.Phase = phase
	provisioner.Status.OrganizationID = organizationID
	provisioner.Status.OrganizationName = organizationName
	provisioner.Status.ProvisionerKeyID = provisionerKeyID
	provisioner.Status.ProvisionerKeyName = provisionerKeyName
	provisioner.Status.TagsHash = tagsHash
	provisioner.Status.ControlPlaneRefName = controlPlaneRefName
	provisioner.Status.ControlPlaneURL = controlPlaneURL
	provisioner.Status.SecretRef = &coderv1alpha1.SecretKeySelector{
		Name: secretRef.Name,
		Key:  secretRef.Key,
	}

	if deployment.Status.ReadyReplicas > 0 {
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionDeploymentReady,
			metav1.ConditionTrue,
			"MinimumReplicasReady",
			"At least one provisioner pod is ready",
		)
	} else {
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionDeploymentReady,
			metav1.ConditionFalse,
			"NoReplicasReady",
			"No provisioner pods are ready yet",
		)
	}

	if equality.Semantic.DeepEqual(*statusSnapshot, provisioner.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, provisioner); err != nil {
		return fmt.Errorf("update coderprovisioner status: %w", err)
	}

	return nil
}

func indexProvisionerByControlPlaneRefName(obj client.Object) []string {
	provisioner, ok := obj.(*coderv1alpha1.CoderProvisioner)
	if !ok {
		return nil
	}

	controlPlaneName := strings.TrimSpace(provisioner.Spec.ControlPlaneRef.Name)
	if controlPlaneName == "" {
		return nil
	}

	return []string{controlPlaneName}
}

func (r *CoderProvisionerReconciler) reconcileRequestsForControlPlane(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	controlPlane, ok := obj.(*coderv1alpha1.CoderControlPlane)
	if !ok {
		return nil
	}
	if strings.TrimSpace(controlPlane.Name) == "" || strings.TrimSpace(controlPlane.Namespace) == "" {
		return nil
	}

	var provisioners coderv1alpha1.CoderProvisionerList
	if err := r.List(
		ctx,
		&provisioners,
		client.InNamespace(controlPlane.Namespace),
		client.MatchingFields{provisionerControlPlaneRefNameFieldIndex: controlPlane.Name},
	); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(provisioners.Items))
	for _, provisioner := range provisioners.Items {
		if strings.TrimSpace(provisioner.Name) == "" || strings.TrimSpace(provisioner.Namespace) == "" {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      provisioner.Name,
			Namespace: provisioner.Namespace,
		}})
	}
	// credentialsecret.Read refuses the Secrets that a control plane uses for
	// its own access, so re-queue every provisioner that references one of them.
	for _, name := range credentialsecret.ControlPlaneSecretNames(controlPlane) {
		requests = append(requests, r.reconcileRequestsForSecret(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlPlane.Namespace}})...)
	}

	return requests
}

// indexProvisionerBySecretNames returns the Secret names that extraEnv
// references, and the provisioner key Secret name.
func indexProvisionerBySecretNames(obj client.Object) []string {
	provisioner, ok := obj.(*coderv1alpha1.CoderProvisioner)
	if !ok {
		return nil
	}
	_, keySecretName, _ := provisionerKeyConfig(provisioner)
	return sortedUnique(append(extraEnvSecretNames(provisioner.Spec.ExtraEnv), keySecretName))
}

func (r *CoderProvisionerReconciler) reconcileRequestsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Name == "" || secret.Namespace == "" {
		return nil
	}

	var provisioners coderv1alpha1.CoderProvisionerList
	if err := r.List(ctx, &provisioners,
		client.InNamespace(secret.Namespace),
		client.MatchingFields{credentialSecretNamesFieldIndex: secret.Name},
	); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list provisioners for secret", "secret", secret.Name, "namespace", secret.Namespace)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(provisioners.Items))
	for _, provisioner := range provisioners.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: provisioner.Name, Namespace: provisioner.Namespace}})
	}
	return requests
}

// SetupWithManager wires the reconciler into controller-runtime.
func (r *CoderProvisionerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("assertion failed: manager must not be nil")
	}
	if r.Client == nil {
		return fmt.Errorf("assertion failed: reconciler client must not be nil")
	}
	if r.Scheme == nil {
		return fmt.Errorf("assertion failed: reconciler scheme must not be nil")
	}
	if r.BootstrapClient == nil {
		return fmt.Errorf("assertion failed: reconciler bootstrap client must not be nil")
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&coderv1alpha1.CoderProvisioner{},
		provisionerControlPlaneRefNameFieldIndex,
		indexProvisionerByControlPlaneRefName,
	); err != nil {
		return fmt.Errorf("index coder provisioners by control plane ref name: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&coderv1alpha1.CoderProvisioner{},
		credentialSecretNamesFieldIndex,
		indexProvisionerBySecretNames,
	); err != nil {
		return fmt.Errorf("index coder provisioners by secret names: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderProvisioner{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(
			&coderv1alpha1.CoderControlPlane{},
			handler.EnqueueRequestsFromMapFunc(r.reconcileRequestsForControlPlane),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.reconcileRequestsForSecret),
			builder.WithPredicates(credentialsecret.WatchPredicate()),
		).
		Named("coderprovisioner").
		Complete(r)
}

func provisionerResourceName(name string) string {
	candidate := provisionerNamePrefix + name
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(provisionerNamePrefix) - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s%s-%s", provisionerNamePrefix, name[:available], suffix)
}

func provisionerLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "coder-provisioner",
		"app.kubernetes.io/instance":   provisionerInstanceLabelValue(name),
		"app.kubernetes.io/managed-by": "coder-k8s",
	}
}

func provisionerInstanceLabelValue(name string) string {
	if len(name) <= 63 {
		return name
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s", name[:available], suffix)
}

func provisionerServiceAccountName(name string) string {
	candidate := fmt.Sprintf("%s%s", name, provisionerServiceAccountSuffix)
	if len(candidate) <= 63 {
		return candidate
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(name))
	suffix := fmt.Sprintf("%08x", hasher.Sum32())
	available := 63 - len(provisionerServiceAccountSuffix) - len(suffix) - 1
	if available < 1 {
		available = 1
	}

	return fmt.Sprintf("%s-%s%s", name[:available], suffix, provisionerServiceAccountSuffix)
}

func provisionerOrganizationName(name string) string {
	if name == "" {
		return defaultProvisionerOrganizationName
	}

	return name
}

func provisionerKeyConfig(provisioner *coderv1alpha1.CoderProvisioner) (string, string, string) {
	keyName := provisioner.Spec.Key.Name
	if keyName == "" {
		keyName = provisioner.Name
	}

	const maxKeyNameLength = 128
	if len(keyName) > maxKeyNameLength {
		hasher := fnv.New32a()
		_, _ = hasher.Write([]byte(keyName))
		suffix := fmt.Sprintf("%08x", hasher.Sum32())
		available := maxKeyNameLength - len(suffix) - 1
		if available < 1 {
			available = 1
		}
		keyName = fmt.Sprintf("%s-%s", keyName[:available], suffix)
	}

	secretName := provisioner.Spec.Key.SecretName
	if secretName == "" {
		const secretNameSuffix = "-provisioner-key"
		candidate := provisioner.Name + secretNameSuffix
		if len(candidate) <= 253 {
			secretName = candidate
		} else {
			hasher := fnv.New32a()
			_, _ = hasher.Write([]byte(provisioner.Name))
			suffix := fmt.Sprintf("%08x", hasher.Sum32())
			available := 253 - len(secretNameSuffix) - len(suffix) - 1
			if available < 1 {
				available = 1
			}
			secretName = fmt.Sprintf("%s-%s%s", provisioner.Name[:available], suffix, secretNameSuffix)
		}
	}

	secretKey := provisioner.Spec.Key.SecretKey
	if secretKey == "" {
		secretKey = coderv1alpha1.DefaultProvisionerKeySecretKey
	}

	return keyName, secretName, secretKey
}

func hashProvisionerTags(tags map[string]string) string {
	keys := slices.Collect(maps.Keys(tags))
	slices.Sort(keys)
	hasher := fnv.New32a()
	for _, key := range keys {
		_, _ = hasher.Write([]byte(key))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(tags[key]))
		_, _ = hasher.Write([]byte{0})
	}

	return fmt.Sprintf("%08x", hasher.Sum32())
}

func hashProvisionerSecret(secretValue []byte) string {
	hasher := fnv.New32a()
	_, _ = hasher.Write(secretValue)
	return fmt.Sprintf("%08x", hasher.Sum32())
}

func nextProvisionerRateLimitBackoff(namespacedName types.NamespacedName) time.Duration {
	if namespacedName.Namespace == "" || namespacedName.Name == "" {
		return provisionerRateLimitBackoffFloor
	}

	mapKey := namespacedName.String()
	attempt := 1
	if previous, ok := provisionerRateLimitBackoffAttempts.Load(mapKey); ok {
		previousAttempt, castOK := previous.(int)
		if !castOK || previousAttempt < 0 {
			previousAttempt = 0
		}
		attempt = previousAttempt + 1
	}
	provisionerRateLimitBackoffAttempts.Store(mapKey, attempt)

	backoff := provisionerRateLimitBackoffBase
	for i := 1; i < attempt; i++ {
		if backoff >= provisionerRateLimitBackoffCap/2 {
			backoff = provisionerRateLimitBackoffCap
			break
		}
		backoff *= 2
	}
	if backoff > provisionerRateLimitBackoffCap {
		backoff = provisionerRateLimitBackoffCap
	}

	jittered := time.Duration(float64(backoff) * provisionerRateLimitJitterMultiplier())
	if jittered < provisionerRateLimitBackoffFloor {
		jittered = provisionerRateLimitBackoffFloor
	}
	if jittered > provisionerRateLimitBackoffCap {
		jittered = provisionerRateLimitBackoffCap
	}

	return jittered
}

func resetProvisionerRateLimitBackoff(namespacedName types.NamespacedName) {
	if namespacedName.Namespace == "" || namespacedName.Name == "" {
		return
	}
	provisionerRateLimitBackoffAttempts.Delete(namespacedName.String())
}

func provisionerRateLimitJitterMultiplier() float64 {
	if provisionerRateLimitJitterRatio <= 0 {
		return 1
	}

	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return 1
	}

	randomFraction := float64(binary.BigEndian.Uint64(randomBytes[:])) / float64(^uint64(0))
	jitter := (randomFraction*2 - 1) * provisionerRateLimitJitterRatio
	return 1 + jitter
}

func durationUntilNextEntitlementsCheck(lastChecked *metav1.Time, interval time.Duration) time.Duration {
	if lastChecked == nil {
		return 0
	}
	if interval <= 0 {
		return 0
	}

	elapsed := time.Since(lastChecked.Time)
	if elapsed < 0 {
		return interval
	}
	if elapsed >= interval {
		return 0
	}

	return interval - elapsed
}

func setCondition(
	provisioner *coderv1alpha1.CoderProvisioner,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) {
	meta.SetStatusCondition(&provisioner.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: provisioner.Generation,
		Reason:             reason,
		Message:            message,
	})
}

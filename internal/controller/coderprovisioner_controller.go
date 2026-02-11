// Package controller contains Kubernetes controllers for coder-k8s resources.
package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/coder/coder-k8s/internal/coderbootstrap"
)

const (
	defaultProvisionerReplicas                      = int32(1)
	defaultProvisionerTerminationGracePeriodSeconds = int64(600)
	defaultProvisionerOrganizationName              = "default"
	provisionerNamePrefix                           = "provisioner-"
	provisionerServiceAccountSuffix                 = "-provisioner"
	provisionerKeyChecksumAnnotation                = "checksum/provisioner-key"
)

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
// +kubebuilder:rbac:groups="",resources=secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges the desired CoderProvisioner spec into Deployment, RBAC, and Secret resources.
func (r *CoderProvisionerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	if err := r.Get(ctx, req.NamespacedName, provisioner); err != nil {
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

	organizationName := provisionerOrganizationName(provisioner.Spec.OrganizationName)
	keyName, keySecretName, keySecretKey := provisionerKeyConfig(provisioner)

	sessionToken, err := r.readBootstrapSessionToken(ctx, provisioner)
	if err != nil {
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionBootstrapSecretReady,
			metav1.ConditionFalse,
			"BootstrapSecretUnavailable",
			fmt.Sprintf("Failed to read bootstrap credentials: %v", err),
		)
		_ = r.Status().Update(ctx, provisioner)
		return ctrl.Result{}, err
	}
	setCondition(
		provisioner,
		coderv1alpha1.CoderProvisionerConditionBootstrapSecretReady,
		metav1.ConditionTrue,
		"BootstrapSecretAvailable",
		"Bootstrap credentials secret is available",
	)

	desiredTagsHash := hashProvisionerTags(provisioner.Spec.Tags)
	status := provisioner.Status
	orgDrift := status.OrganizationName != "" && status.OrganizationName != organizationName
	keyNameDrift := status.ProvisionerKeyName != "" && status.ProvisionerKeyName != keyName
	tagsDrift := status.TagsHash != "" && status.TagsHash != desiredTagsHash
	driftDetected := orgDrift || keyNameDrift || tagsDrift
	appliedOrgName := provisioner.Status.OrganizationName
	appliedTagsHash := provisioner.Status.TagsHash

	// Check whether a usable provisioner key secret already exists.
	// The secret is considered "usable" only if the Secret object exists
	// AND it contains a non-empty value at the configured data key.
	secretNamespacedName := types.NamespacedName{Name: keySecretName, Namespace: provisioner.Namespace}
	existingSecret := &corev1.Secret{}
	secretUsable := false
	if err := r.Get(ctx, secretNamespacedName, existingSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get provisioner key secret %s: %w", secretNamespacedName, err)
		}
	} else {
		secretUsable = len(existingSecret.Data[keySecretKey]) > 0
	}

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
			"orgDrift", orgDrift, "keyNameDrift", keyNameDrift, "tagsDrift", tagsDrift)

		oldOrg := provisioner.Status.OrganizationName
		if oldOrg == "" {
			oldOrg = organizationName
		}
		oldKeyName := provisioner.Status.ProvisionerKeyName
		if oldKeyName == "" {
			oldKeyName = keyName
		}

		if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
			ctx,
			controlPlane.Status.URL,
			sessionToken,
			oldOrg,
			oldKeyName,
		); deleteErr != nil {
			log.Info("failed to delete old provisioner key during drift rotation, creating new key anyway",
				"oldKeyName", oldKeyName, "error", deleteErr)
		}

		response, ensureErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
			CoderURL:         controlPlane.Status.URL,
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
				ctx, controlPlane.Status.URL, sessionToken, organizationName, keyName,
			); deleteErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to delete provisioner key %q for drift recovery", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("delete provisioner key %q for drift recovery: %w", keyName, deleteErr)
			}
			rotated, rotateErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL:         controlPlane.Status.URL,
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
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
			metav1.ConditionTrue,
			"ProvisionerKeyReady",
			"Provisioner key is available in coderd",
		)
	} else if !secretUsable {
		response, ensureErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
			CoderURL:         controlPlane.Status.URL,
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
				ctx, controlPlane.Status.URL, sessionToken, organizationName, keyName,
			); deleteErr != nil {
				setCondition(provisioner, coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
					metav1.ConditionFalse, "ProvisionerKeyFailed",
					fmt.Sprintf("Failed to delete stale provisioner key %q for rotation", keyName))
				_ = r.Status().Update(ctx, provisioner)
				return ctrl.Result{}, fmt.Errorf("delete stale provisioner key %q for rotation: %w", keyName, deleteErr)
			}
			rotated, rotateErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
				CoderURL:         controlPlane.Status.URL,
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
		setCondition(
			provisioner,
			coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
			metav1.ConditionTrue,
			"ProvisionerKeyReady",
			"Provisioner key is available in coderd",
		)
	} else if status.OrganizationName == "" || status.TagsHash == "" {
		// Secret is usable and no drift detected, but status metadata is empty
		// (e.g. upgrade from older version). Call EnsureProvisionerKey to populate
		// IDs and key name. If coderd reports an existing key (no plaintext key
		// returned), rotate it best-effort so desired tags are applied before
		// stamping the metadata baseline.
		response, ensureErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
			CoderURL:         controlPlane.Status.URL,
			SessionToken:     sessionToken,
			OrganizationName: organizationName,
			KeyName:          keyName,
			Tags:             provisioner.Spec.Tags,
		})
		if ensureErr != nil {
			log.Info("failed to verify provisioner key metadata, will retry",
				"keyName", keyName, "error", ensureErr)
		} else {
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
			} else {
				// Key already exists; tags may be stale. Rotate to ensure desired tags are applied.
				log.Info("existing key found during metadata backfill, rotating to ensure desired tags",
					"keyName", keyName)
				if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
					ctx, controlPlane.Status.URL, sessionToken, organizationName, keyName,
				); deleteErr != nil {
					log.Info("failed to delete key for metadata backfill rotation, will retry",
						"keyName", keyName, "error", deleteErr)
				} else {
					rotated, rotateErr := r.BootstrapClient.EnsureProvisionerKey(ctx, coderbootstrap.EnsureProvisionerKeyRequest{
						CoderURL:         controlPlane.Status.URL,
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
					appliedOrgName = organizationName
					appliedTagsHash = desiredTagsHash
				}
			}
			setCondition(
				provisioner,
				coderv1alpha1.CoderProvisionerConditionProvisionerKeyReady,
				metav1.ConditionTrue,
				"ProvisionerKeyReady",
				"Provisioner key is available in coderd",
			)
		}
	}

	provisionerKeySecret, err := r.ensureProvisionerKeySecret(ctx, provisioner, keySecretName, keySecretKey, keyMaterial)
	if err != nil {
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
	deployment, err := r.reconcileDeployment(ctx, provisioner, image, controlPlane.Status.URL, organizationName, secretRef, serviceAccountName, secretChecksum)
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

	// Use the last-applied organization name from status so we target the
	// correct org even if spec was changed but never successfully rotated.
	// Fall back to the spec-derived name only when status is empty.
	organizationName := provisioner.Status.OrganizationName
	if organizationName == "" {
		organizationName = provisionerOrganizationName(provisioner.Spec.OrganizationName)
	}
	keyName := provisioner.Status.ProvisionerKeyName
	if keyName == "" {
		keyName, _, _ = provisionerKeyConfig(provisioner)
	}

	// Best-effort remote key cleanup: if the referenced control plane,
	// its URL, bootstrap credentials, or any other prerequisite is
	// unavailable, log a warning and proceed to finalizer removal so the
	// CR does not get stuck in Terminating. This is common during
	// namespace teardown, when the control plane was never ready, or
	// when credentials were misconfigured.
	controlPlane, err := r.fetchControlPlane(ctx, provisioner)
	if err != nil {
		log.Info("unable to reach referenced CoderControlPlane during deletion, skipping remote key cleanup",
			"controlPlaneRef", provisioner.Spec.ControlPlaneRef.Name, "error", err)
	} else {
		sessionToken, tokenErr := r.readBootstrapSessionToken(ctx, provisioner)
		if tokenErr != nil {
			log.Info("unable to read bootstrap credentials during deletion, skipping remote key cleanup",
				"credentialsSecretRef", provisioner.Spec.Bootstrap.CredentialsSecretRef.Name, "error", tokenErr)
		} else {
			if deleteErr := r.BootstrapClient.DeleteProvisionerKey(
				ctx,
				controlPlane.Status.URL,
				sessionToken,
				organizationName,
				keyName,
			); deleteErr != nil {
				// Treat key deletion failures as best-effort so the
				// finalizer is still removed. Transient errors, auth
				// issues, or org-lookup failures should not block CR
				// cleanup.
				log.Info("failed to delete remote provisioner key during deletion, proceeding with finalizer removal",
					"keyName", keyName, "error", deleteErr)
			}
		}
	}

	controllerutil.RemoveFinalizer(provisioner, coderv1alpha1.ProvisionerKeyCleanupFinalizer)
	if err := r.Update(ctx, provisioner); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from coderprovisioner %s/%s: %w", provisioner.Namespace, provisioner.Name, err)
	}

	return ctrl.Result{}, nil
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

func (r *CoderProvisionerReconciler) readBootstrapSessionToken(ctx context.Context, provisioner *coderv1alpha1.CoderProvisioner) (string, error) {
	credentialsRef := provisioner.Spec.Bootstrap.CredentialsSecretRef
	if credentialsRef.Name == "" {
		return "", fmt.Errorf("coderprovisioner %s/%s spec.bootstrap.credentialsSecretRef.name is required", provisioner.Namespace, provisioner.Name)
	}

	credentialsKey := credentialsRef.Key
	if credentialsKey == "" {
		credentialsKey = coderv1alpha1.DefaultTokenSecretKey
	}

	token, err := r.readSecretValue(ctx, provisioner.Namespace, credentialsRef.Name, credentialsKey)
	if err != nil {
		return "", fmt.Errorf("read bootstrap credentials secret %q/%q key %q: %w", provisioner.Namespace, credentialsRef.Name, credentialsKey, err)
	}

	return token, nil
}

func (r *CoderProvisionerReconciler) ensureProvisionerKeySecret(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	secretName string,
	secretKey string,
	keyMaterial string,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: provisioner.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		labels := provisionerLabels(provisioner.Name)
		secret.Labels = maps.Clone(labels)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		if keyMaterial != "" {
			secret.Data[secretKey] = []byte(keyMaterial)
		}
		if len(secret.Data[secretKey]) == 0 {
			return fmt.Errorf("provisioner key secret %q key %q is empty", secretName, secretKey)
		}
		if err := controllerutil.SetControllerReference(provisioner, secret, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile provisioner key secret %q: %w", secretName, err)
	}

	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: provisioner.Namespace}, secret); err != nil {
		return nil, fmt.Errorf("get reconciled provisioner key secret %q: %w", secretName, err)
	}

	return secret, nil
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

	if err := r.Get(ctx, types.NamespacedName{Name: serviceAccount.Name, Namespace: serviceAccount.Namespace}, serviceAccount); err != nil {
		return nil, fmt.Errorf("get reconciled serviceaccount %q: %w", serviceAccountName, err)
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

	if err := r.Get(ctx, types.NamespacedName{Name: role.Name, Namespace: role.Namespace}, role); err != nil {
		return nil, fmt.Errorf("get reconciled role %q: %w", roleName, err)
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

	if err := r.Get(ctx, types.NamespacedName{Name: roleBinding.Name, Namespace: roleBinding.Namespace}, roleBinding); err != nil {
		return nil, fmt.Errorf("get reconciled rolebinding %q: %w", roleBindingName, err)
	}

	return roleBinding, nil
}

func (r *CoderProvisionerReconciler) reconcileDeployment(
	ctx context.Context,
	provisioner *coderv1alpha1.CoderProvisioner,
	image string,
	coderURL string,
	organizationName string,
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

		args := []string{"provisionerd", "start"}
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
		if organizationName != "" && organizationName != defaultProvisionerOrganizationName {
			env = append(env, corev1.EnvVar{Name: "CODER_ORGANIZATION", Value: organizationName})
		}
		env = append(env, provisioner.Spec.ExtraEnv...)

		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}
		deployment.Spec.Template = corev1.PodTemplateSpec{
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
					Args:      args,
					Env:       env,
					Resources: provisioner.Spec.Resources,
				}},
			},
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile provisioner deployment: %w", err)
	}

	if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, deployment); err != nil {
		return nil, fmt.Errorf("get reconciled deployment %q: %w", deployment.Name, err)
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
) error {
	phase := coderv1alpha1.CoderProvisionerPhasePending
	if deployment.Status.ReadyReplicas > 0 {
		phase = coderv1alpha1.CoderProvisionerPhaseReady
	}

	provisioner.Status.ObservedGeneration = provisioner.Generation
	provisioner.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	provisioner.Status.Phase = phase
	provisioner.Status.OrganizationID = organizationID
	provisioner.Status.OrganizationName = organizationName
	provisioner.Status.ProvisionerKeyID = provisionerKeyID
	provisioner.Status.ProvisionerKeyName = provisionerKeyName
	provisioner.Status.TagsHash = tagsHash
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

	if err := r.Status().Update(ctx, provisioner); err != nil {
		return fmt.Errorf("update coderprovisioner status: %w", err)
	}

	return nil
}

func (r *CoderProvisionerReconciler) readSecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		return "", err
	}

	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %q does not contain key %q", name, key)
	}
	if len(value) == 0 {
		return "", fmt.Errorf("secret %q key %q is empty", name, key)
	}

	return string(value), nil
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&coderv1alpha1.CoderProvisioner{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
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

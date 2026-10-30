package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Cleanup records (#353 part 2, option B′). Before the operator creates
// something in Coder for an object, it writes a record: one ConfigMap named
// ck8s-cleanup-<object UID> in the operator's own namespace. The record says
// which Coder deployment holds the resource and how to find it. It stays when
// the object goes without a confirmed delete in Coder (for example in a
// namespace deletion), so a later sweep through a control plane of the same
// deployment can delete the resource. A record holds no secrets. The
// ConfigMap creation time is the record's created-at.
const (
	cleanupRecordPrefix = "ck8s-cleanup-"
	// cleanupRecordLabel marks a record. Its value is the object kind.
	cleanupRecordLabel = "coder.com/cleanup-record"

	cleanupRecordKind         = "kind"
	cleanupRecordUID          = "uid"
	cleanupRecordNamespace    = "namespace"
	cleanupRecordName         = "name"
	cleanupRecordDeploymentID = "deploymentID"
	cleanupRecordNonce        = "nonce"
	cleanupRecordCoderID      = "coderID"
	cleanupRecordType         = "type"
	cleanupRecordOrganization = "organizationID"
	cleanupRecordSlug         = "slug"
	cleanupRecordCreateSentAt = "createAttemptTime"
)

// cleanupRecord is the content of a record.
type cleanupRecord struct {
	Kind      string
	UID       types.UID
	Namespace string
	Name      string
	// DeploymentID is Coder's deployment ID (/api/v2/buildinfo), read from
	// Coder in the reconcile that sends the create. It lives in the Coder
	// database, so it names the database that holds the resource.
	DeploymentID string
	// Nonce finds a resource whose Coder ID is not known yet: the display
	// name of a provider or of an MCP server config.
	Nonce string
	// CoderID is the Coder ID of the resource once the operator knows it.
	CoderID string
	// Type is the provider type that the create sent. With the nonce it
	// proves that a provider found by name is the object's.
	Type string
	// Organization and Slug locate an MCP server config: its Coder
	// organization ID and its slug in that organization.
	Organization string
	Slug         string
	// CreateAttemptTime is when the operator last sent a create. A sweep
	// waits for the settle window after it.
	CreateAttemptTime time.Time
}

func cleanupRecordObjectName(uid types.UID) string { return cleanupRecordPrefix + string(uid) }

// errNotACleanupRecord marks a ConfigMap of a record's name that is not a
// valid record of that object. The operator never writes over it or deletes it.
var errNotACleanupRecord = errors.New("not a cleanup record of this object")

// coderDeploymentID reads Coder's deployment ID. Only a canonical UUID is
// accepted: the ID decides which resources a sweep may delete.
func coderDeploymentID(ctx context.Context, sdk *codersdk.Client) (string, error) {
	info, err := sdk.BuildInfo(ctx)
	if err != nil {
		return "", err
	}
	if id, err := uuid.Parse(info.DeploymentID); err != nil || id == uuid.Nil || id.String() != info.DeploymentID {
		return "", fmt.Errorf("coder answered a deployment ID %q that is not a canonical UUID", info.DeploymentID)
	}
	return info.DeploymentID, nil
}

// readCleanupRecord returns the record of uid, or nil when there is none. A
// ConfigMap of that name that is not a record of uid is an error: the
// operator never writes over it.
func readCleanupRecord(ctx context.Context, reader client.Reader, namespace string, uid types.UID) (*cleanupRecord, *corev1.ConfigMap, error) {
	if namespace == "" || uid == "" {
		return nil, nil, errors.New("assertion failed: a cleanup record needs the operator namespace and an object UID")
	}
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cleanupRecordObjectName(uid)}, configMap); apierrors.IsNotFound(err) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("get cleanup record %s/%s: %w", namespace, cleanupRecordObjectName(uid), err)
	}
	record, err := parseCleanupRecord(configMap)
	if err != nil {
		return nil, nil, err
	}
	if record.UID != uid {
		return nil, nil, fmt.Errorf("ConfigMap %s/%s is not the cleanup record of object %s: %w", namespace, configMap.Name, uid, errNotACleanupRecord)
	}
	return record, configMap, nil
}

// parseCleanupRecord reads a record from its ConfigMap.
func parseCleanupRecord(configMap *corev1.ConfigMap) (*cleanupRecord, error) {
	d := configMap.Data
	record := &cleanupRecord{
		Kind: d[cleanupRecordKind], UID: types.UID(d[cleanupRecordUID]), Namespace: d[cleanupRecordNamespace], Name: d[cleanupRecordName],
		DeploymentID: d[cleanupRecordDeploymentID], Nonce: d[cleanupRecordNonce], CoderID: d[cleanupRecordCoderID], Type: d[cleanupRecordType],
		Organization: d[cleanupRecordOrganization], Slug: d[cleanupRecordSlug],
	}
	if configMap.Labels[cleanupRecordLabel] == "" || configMap.Labels[cleanupRecordLabel] != record.Kind || record.UID == "" ||
		configMap.Name != cleanupRecordObjectName(record.UID) || record.DeploymentID == "" {
		return nil, fmt.Errorf("ConfigMap %s/%s is not a valid cleanup record: %w", configMap.Namespace, configMap.Name, errNotACleanupRecord)
	}
	if t := d[cleanupRecordCreateSentAt]; t != "" {
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return nil, fmt.Errorf("cleanup record %s/%s has an invalid %s (%w): %w", configMap.Namespace, configMap.Name, cleanupRecordCreateSentAt, err, errNotACleanupRecord)
		}
		record.CreateAttemptTime = parsed
	}
	return record, nil
}

// writeCleanupRecord creates or replaces the record in namespace. existing
// is the ConfigMap that readCleanupRecord returned, or nil.
func writeCleanupRecord(ctx context.Context, c client.Client, namespace string, existing *corev1.ConfigMap, record cleanupRecord) error {
	if namespace == "" || record.Kind == "" || record.UID == "" || record.DeploymentID == "" {
		return errors.New("assertion failed: a cleanup record needs the operator namespace, a kind, an object UID and a deployment ID")
	}
	data := map[string]string{
		cleanupRecordKind: record.Kind, cleanupRecordUID: string(record.UID), cleanupRecordNamespace: record.Namespace, cleanupRecordName: record.Name,
		cleanupRecordDeploymentID: record.DeploymentID, cleanupRecordNonce: record.Nonce, cleanupRecordCoderID: record.CoderID, cleanupRecordType: record.Type,
	}
	if record.Organization != "" || record.Slug != "" {
		data[cleanupRecordOrganization], data[cleanupRecordSlug] = record.Organization, record.Slug
	}
	if !record.CreateAttemptTime.IsZero() {
		data[cleanupRecordCreateSentAt] = record.CreateAttemptTime.UTC().Format(time.RFC3339)
	}
	if existing != nil {
		updated := existing.DeepCopy()
		updated.Data = data
		if err := c.Update(ctx, updated); err != nil {
			return fmt.Errorf("update cleanup record %s/%s: %w", namespace, updated.Name, err)
		}
		return nil
	}
	created := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: cleanupRecordObjectName(record.UID), Namespace: namespace,
			Labels: map[string]string{cleanupRecordLabel: record.Kind, "app.kubernetes.io/managed-by": "coder-k8s"},
		},
		Data: data,
	}
	if err := c.Create(ctx, created); err != nil {
		return fmt.Errorf("create cleanup record %s/%s: %w", namespace, created.Name, err)
	}
	return nil
}

// deleteCleanupRecord removes the record of uid. Call it only after Coder
// confirmed that it holds no resource of the object. It reads the record
// through reader (live) and deletes only that ConfigMap (UID precondition).
// A ConfigMap of that name that is not a record of uid stays.
func deleteCleanupRecord(ctx context.Context, c client.Client, reader client.Reader, namespace string, uid types.UID) error {
	record, configMap, err := readCleanupRecord(ctx, reader, namespace, uid)
	if errors.Is(err, errNotACleanupRecord) {
		log.FromContext(ctx).Error(err, "Leaving a ConfigMap that is not the cleanup record of this object")
		return nil
	} else if err != nil || record == nil {
		return err
	}
	if err := c.Delete(ctx, configMap, client.Preconditions{UID: &configMap.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete cleanup record %s/%s: %w", namespace, configMap.Name, err)
	}
	return nil
}

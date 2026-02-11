package coder

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/coder/coder/v2/codersdk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ControlPlaneClientProvider resolves Coder SDK clients from eligible CoderControlPlane resources.
type ControlPlaneClientProvider struct {
	cpReader       client.Reader
	secretReader   client.Reader
	requestTimeout time.Duration
}

var _ ClientProvider = (*ControlPlaneClientProvider)(nil)

// NewControlPlaneClientProvider constructs a dynamic ClientProvider backed by CoderControlPlane resources.
func NewControlPlaneClientProvider(
	cpReader client.Reader,
	secretReader client.Reader,
	requestTimeout time.Duration,
) (*ControlPlaneClientProvider, error) {
	if cpReader == nil {
		return nil, fmt.Errorf("assertion failed: control plane reader must not be nil")
	}
	if secretReader == nil {
		return nil, fmt.Errorf("assertion failed: secret reader must not be nil")
	}
	if requestTimeout < 0 {
		return nil, fmt.Errorf("assertion failed: request timeout must not be negative")
	}

	provider := &ControlPlaneClientProvider{
		cpReader:       cpReader,
		secretReader:   secretReader,
		requestTimeout: requestTimeout,
	}
	if provider.cpReader == nil {
		return nil, fmt.Errorf("assertion failed: control plane reader is nil after successful construction")
	}
	if provider.secretReader == nil {
		return nil, fmt.Errorf("assertion failed: secret reader is nil after successful construction")
	}

	return provider, nil
}

// ClientForNamespace resolves an SDK client from one eligible CoderControlPlane.
func (p *ControlPlaneClientProvider) ClientForNamespace(ctx context.Context, namespace string) (*codersdk.Client, error) {
	if p == nil {
		return nil, fmt.Errorf("assertion failed: control plane client provider must not be nil")
	}
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if p.cpReader == nil {
		return nil, fmt.Errorf("assertion failed: control plane reader must not be nil")
	}
	if p.secretReader == nil {
		return nil, fmt.Errorf("assertion failed: secret reader must not be nil")
	}

	controlPlaneList := &coderv1alpha1.CoderControlPlaneList{}
	listOptions := make([]client.ListOption, 0, 1)
	if namespace != "" {
		listOptions = append(listOptions, client.InNamespace(namespace))
	}
	if err := p.cpReader.List(ctx, controlPlaneList, listOptions...); err != nil {
		if namespace == "" {
			return nil, fmt.Errorf("list CoderControlPlane resources across all namespaces: %w", err)
		}

		return nil, fmt.Errorf("list CoderControlPlane resources in namespace %q: %w", namespace, err)
	}

	eligible := make([]coderv1alpha1.CoderControlPlane, 0, 1)
	for i := range controlPlaneList.Items {
		controlPlane := controlPlaneList.Items[i]
		if strings.Contains(controlPlane.Name, ".") {
			log.Printf(
				"warning: skipping CoderControlPlane %s/%s: names containing '.' are incompatible with aggregated naming",
				controlPlane.Namespace,
				controlPlane.Name,
			)
			continue
		}
		if controlPlane.Spec.OperatorAccess.Disabled {
			continue
		}
		if !controlPlane.Status.OperatorAccessReady {
			continue
		}
		if controlPlane.Status.OperatorTokenSecretRef == nil {
			continue
		}
		if strings.TrimSpace(controlPlane.Status.URL) == "" {
			continue
		}

		eligible = append(eligible, controlPlane)
	}

	switch len(eligible) {
	case 0:
		return nil, apierrors.NewServiceUnavailable(noEligibleControlPlaneMessage(namespace))
	case 1:
		// handled below
	default:
		return nil, apierrors.NewBadRequest(multipleEligibleControlPlaneMessage(namespace))
	}

	controlPlane := eligible[0]
	if controlPlane.Status.OperatorTokenSecretRef == nil {
		return nil, fmt.Errorf("assertion failed: eligible CoderControlPlane is missing status.operatorTokenSecretRef")
	}

	secretName := strings.TrimSpace(controlPlane.Status.OperatorTokenSecretRef.Name)
	if secretName == "" {
		return nil, apierrors.NewServiceUnavailable(
			fmt.Sprintf(
				"eligible CoderControlPlane %s/%s is missing status.operatorTokenSecretRef.name",
				controlPlane.Namespace,
				controlPlane.Name,
			),
		)
	}

	secretKey := strings.TrimSpace(controlPlane.Status.OperatorTokenSecretRef.Key)
	if secretKey == "" {
		secretKey = coderv1alpha1.DefaultTokenSecretKey
	}

	tokenSecret := &corev1.Secret{}
	if err := p.secretReader.Get(
		ctx,
		client.ObjectKey{Namespace: controlPlane.Namespace, Name: secretName},
		tokenSecret,
	); err != nil {
		return nil, fmt.Errorf(
			"read operator token secret %s/%s for CoderControlPlane %s/%s: %w",
			controlPlane.Namespace,
			secretName,
			controlPlane.Namespace,
			controlPlane.Name,
			err,
		)
	}

	tokenBytes, ok := tokenSecret.Data[secretKey]
	if !ok {
		return nil, apierrors.NewServiceUnavailable(
			fmt.Sprintf(
				"operator token secret %s/%s for CoderControlPlane %s/%s does not contain key %q",
				controlPlane.Namespace,
				secretName,
				controlPlane.Namespace,
				controlPlane.Name,
				secretKey,
			),
		)
	}

	sessionToken := string(tokenBytes)
	if sessionToken == "" {
		return nil, apierrors.NewServiceUnavailable(
			fmt.Sprintf(
				"operator token secret %s/%s for CoderControlPlane %s/%s contains an empty value for key %q",
				controlPlane.Namespace,
				secretName,
				controlPlane.Namespace,
				controlPlane.Name,
				secretKey,
			),
		)
	}

	coderURL := strings.TrimSpace(controlPlane.Status.URL)
	parsedCoderURL, err := url.Parse(coderURL)
	if err != nil {
		return nil, fmt.Errorf(
			"parse CoderControlPlane URL %q for %s/%s: %w",
			coderURL,
			controlPlane.Namespace,
			controlPlane.Name,
			err,
		)
	}
	if parsedCoderURL == nil {
		return nil, fmt.Errorf("assertion failed: parsed CoderControlPlane URL must not be nil")
	}

	sdkClient, err := NewSDKClient(Config{
		CoderURL:       parsedCoderURL,
		SessionToken:   sessionToken,
		RequestTimeout: p.requestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"construct Coder SDK client for CoderControlPlane %s/%s: %w",
			controlPlane.Namespace,
			controlPlane.Name,
			err,
		)
	}
	if sdkClient == nil {
		return nil, fmt.Errorf("assertion failed: Coder SDK client is nil after successful construction")
	}

	return sdkClient, nil
}

func noEligibleControlPlaneMessage(namespace string) string {
	if namespace == "" {
		return "no eligible CoderControlPlane instances found across all namespaces"
	}

	return fmt.Sprintf("no eligible CoderControlPlane instances found in namespace %q", namespace)
}

func multipleEligibleControlPlaneMessage(namespace string) string {
	if namespace == "" {
		return "multiple eligible CoderControlPlane instances across namespaces; multi-instance support is planned"
	}

	return fmt.Sprintf(
		"multiple eligible CoderControlPlane instances in namespace %q; multi-instance support is planned",
		namespace,
	)
}

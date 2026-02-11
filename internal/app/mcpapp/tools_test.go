package mcpapp

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestListControlPlanePods(t *testing.T) {
	t.Helper()

	labelsForAlpha := controlPlaneWorkloadLabels("alpha")
	startTime := metav1.NewTime(time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC))

	k8sClient := mustNewFakeClient(
		t,
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "alpha-1", Namespace: "default", Labels: labelsForAlpha},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
			},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				StartTime:         &startTime,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: true}, {Name: "sidecar", Ready: false}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "alpha-2", Namespace: "default", Labels: labelsForAlpha},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			Status: corev1.PodStatus{
				Phase:             corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: false}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", Labels: controlPlaneWorkloadLabels("beta")},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
		},
	)

	output, err := listControlPlanePods(context.Background(), k8sClient, listControlPlanePodsInput{
		Namespace: "default",
		Name:      "alpha",
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("list control plane pods: %v", err)
	}
	if len(output.Items) != 1 {
		t.Fatalf("expected one item due to limit, got %d", len(output.Items))
	}
	if !output.Truncated {
		t.Fatal("expected truncated output when limit is smaller than matching pods")
	}

	pod := output.Items[0]
	if pod.Name != "alpha-1" {
		t.Fatalf("expected sorted first pod alpha-1, got %q", pod.Name)
	}
	if pod.ReadyContainers != 1 || pod.TotalContainers != 2 {
		t.Fatalf("expected readiness summary 1/2, got %d/%d", pod.ReadyContainers, pod.TotalContainers)
	}
	if pod.StartTime == "" {
		t.Fatal("expected start time to be populated")
	}
}

func TestGetControlPlaneDeploymentStatus(t *testing.T) {
	t.Helper()

	replicas := int32(3)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:       2,
			UpdatedReplicas:     3,
			AvailableReplicas:   2,
			UnavailableReplicas: 1,
			ObservedGeneration:  7,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
			},
		},
	}

	k8sClient := mustNewFakeClient(t, deployment)

	output, err := getControlPlaneDeploymentStatus(context.Background(), k8sClient, getControlPlaneDeploymentStatusInput{
		Namespace: "default",
		Name:      "alpha",
	})
	if err != nil {
		t.Fatalf("get deployment status: %v", err)
	}

	if output.Replicas != 3 || output.ReadyReplicas != 2 || output.ObservedGeneration != 7 {
		t.Fatalf("unexpected deployment summary: %+v", output)
	}
	if len(output.Conditions) != 2 {
		t.Fatalf("expected two deployment conditions, got %d", len(output.Conditions))
	}
	if output.Conditions[0].Type != string(appsv1.DeploymentAvailable) {
		t.Fatalf("expected sorted conditions with %q first, got %+v", appsv1.DeploymentAvailable, output.Conditions)
	}
}

func TestGetServiceStatus(t *testing.T) {
	t.Helper()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alpha",
			Namespace: "default",
			Annotations: map[string]string{
				"service.beta.kubernetes.io/aws-load-balancer-type": "nlb",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeLoadBalancer,
			ClusterIP: "10.96.0.42",
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt(3000),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	k8sClient := mustNewFakeClient(t, service)

	output, err := getServiceStatus(context.Background(), k8sClient, getServiceStatusInput{
		Namespace: "default",
		Name:      "alpha",
	})
	if err != nil {
		t.Fatalf("get service status: %v", err)
	}

	if output.Type != string(corev1.ServiceTypeLoadBalancer) {
		t.Fatalf("expected service type load balancer, got %q", output.Type)
	}
	if len(output.Ports) != 1 {
		t.Fatalf("expected one service port, got %d", len(output.Ports))
	}
	if output.Ports[0].TargetPort != "3000" {
		t.Fatalf("expected target port 3000, got %q", output.Ports[0].TargetPort)
	}
	if output.Annotations["service.beta.kubernetes.io/aws-load-balancer-type"] != "nlb" {
		t.Fatalf("expected annotation copy, got %+v", output.Annotations)
	}
}

func TestWorkspaceToolHelpers(t *testing.T) {
	t.Helper()

	deadline := metav1.NewTime(time.Date(2026, time.March, 1, 8, 0, 0, 0, time.UTC))
	workspace := &aggregationv1alpha1.CoderWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
		Spec:       aggregationv1alpha1.CoderWorkspaceSpec{Running: false},
		Status:     aggregationv1alpha1.CoderWorkspaceStatus{AutoShutdown: &deadline},
	}

	k8sClient := mustNewFakeClient(t, workspace)
	ctx := context.Background()

	detail, err := getWorkspaceDetail(ctx, k8sClient, getWorkspaceInput{Namespace: "default", Name: "dev"})
	if err != nil {
		t.Fatalf("get workspace detail: %v", err)
	}
	if detail.Running {
		t.Fatalf("expected workspace to be stopped, got %+v", detail)
	}

	setOutput, err := setWorkspaceRunning(ctx, k8sClient, setWorkspaceRunningInput{Namespace: "default", Name: "dev", Running: true})
	if err != nil {
		t.Fatalf("set workspace running: %v", err)
	}
	if !setOutput.Updated || !setOutput.Workspace.Running {
		t.Fatalf("expected workspace update to running=true, got %+v", setOutput)
	}

	persisted := &aggregationv1alpha1.CoderWorkspace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "dev"}, persisted); err != nil {
		t.Fatalf("get persisted workspace: %v", err)
	}
	if !persisted.Spec.Running {
		t.Fatalf("expected persisted running=true, got %+v", persisted.Spec)
	}

	noOpOutput, err := setWorkspaceRunning(ctx, k8sClient, setWorkspaceRunningInput{Namespace: "default", Name: "dev", Running: true})
	if err != nil {
		t.Fatalf("set workspace running no-op: %v", err)
	}
	if noOpOutput.Updated {
		t.Fatalf("expected no-op update to report Updated=false, got %+v", noOpOutput)
	}
}

func TestTemplateToolHelpers(t *testing.T) {
	t.Helper()

	template := &aggregationv1alpha1.CoderTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "starter", Namespace: "default"},
		Spec:       aggregationv1alpha1.CoderTemplateSpec{Running: true},
	}

	k8sClient := mustNewFakeClient(t, template)
	ctx := context.Background()

	detail, err := getTemplateDetail(ctx, k8sClient, getTemplateInput{Namespace: "default", Name: "starter"})
	if err != nil {
		t.Fatalf("get template detail: %v", err)
	}
	if !detail.Running {
		t.Fatalf("expected template to be running, got %+v", detail)
	}

	setOutput, err := setTemplateRunning(ctx, k8sClient, setTemplateRunningInput{Namespace: "default", Name: "starter", Running: false})
	if err != nil {
		t.Fatalf("set template running: %v", err)
	}
	if !setOutput.Updated || setOutput.Template.Running {
		t.Fatalf("expected template update to running=false, got %+v", setOutput)
	}

	persisted := &aggregationv1alpha1.CoderTemplate{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "starter"}, persisted); err != nil {
		t.Fatalf("get persisted template: %v", err)
	}
	if persisted.Spec.Running {
		t.Fatalf("expected persisted running=false, got %+v", persisted.Spec)
	}
}

func TestListControlPlanePodsInputValidation(t *testing.T) {
	t.Helper()

	k8sClient := mustNewFakeClient(t)

	_, err := listControlPlanePods(context.Background(), k8sClient, listControlPlanePodsInput{Name: "alpha"})
	if err == nil {
		t.Fatal("expected error when namespace is missing")
	}

	_, err = listControlPlanePods(context.Background(), k8sClient, listControlPlanePodsInput{Namespace: "default"})
	if err == nil {
		t.Fatal("expected error when name is missing")
	}
}

func mustNewFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := newScheme()
	if scheme == nil {
		t.Fatal("expected non-nil scheme")
	}

	stub := &stubClient{
		scheme:      scheme,
		pods:        map[types.NamespacedName]*corev1.Pod{},
		deployments: map[types.NamespacedName]*appsv1.Deployment{},
		services:    map[types.NamespacedName]*corev1.Service{},
		workspaces:  map[types.NamespacedName]*aggregationv1alpha1.CoderWorkspace{},
		templates:   map[types.NamespacedName]*aggregationv1alpha1.CoderTemplate{},
	}

	for _, object := range objects {
		if object == nil {
			continue
		}
		key := types.NamespacedName{Namespace: object.GetNamespace(), Name: object.GetName()}
		switch typed := object.(type) {
		case *corev1.Pod:
			stub.pods[key] = typed.DeepCopy()
		case *appsv1.Deployment:
			stub.deployments[key] = typed.DeepCopy()
		case *corev1.Service:
			stub.services[key] = typed.DeepCopy()
		case *aggregationv1alpha1.CoderWorkspace:
			stub.workspaces[key] = typed.DeepCopy()
		case *aggregationv1alpha1.CoderTemplate:
			stub.templates[key] = typed.DeepCopy()
		default:
			t.Fatalf("unsupported object type for stub client: %T", object)
		}
	}

	return stub
}

type stubClient struct {
	scheme      *runtime.Scheme
	pods        map[types.NamespacedName]*corev1.Pod
	deployments map[types.NamespacedName]*appsv1.Deployment
	services    map[types.NamespacedName]*corev1.Service
	workspaces  map[types.NamespacedName]*aggregationv1alpha1.CoderWorkspace
	templates   map[types.NamespacedName]*aggregationv1alpha1.CoderTemplate
}

func (s *stubClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if obj == nil {
		return fmt.Errorf("assertion failed: object must not be nil")
	}

	namespacedName := types.NamespacedName(key)
	switch typed := obj.(type) {
	case *corev1.Pod:
		stored, ok := s.pods[namespacedName]
		if !ok {
			return newCoreNotFound("pods", key.Name)
		}
		*typed = *stored.DeepCopy()
		return nil
	case *appsv1.Deployment:
		stored, ok := s.deployments[namespacedName]
		if !ok {
			return newAppsNotFound("deployments", key.Name)
		}
		*typed = *stored.DeepCopy()
		return nil
	case *corev1.Service:
		stored, ok := s.services[namespacedName]
		if !ok {
			return newCoreNotFound("services", key.Name)
		}
		*typed = *stored.DeepCopy()
		return nil
	case *aggregationv1alpha1.CoderWorkspace:
		stored, ok := s.workspaces[namespacedName]
		if !ok {
			return apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), key.Name)
		}
		*typed = *stored.DeepCopy()
		return nil
	case *aggregationv1alpha1.CoderTemplate:
		stored, ok := s.templates[namespacedName]
		if !ok {
			return apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), key.Name)
		}
		*typed = *stored.DeepCopy()
		return nil
	default:
		return fmt.Errorf("assertion failed: unsupported object type %T", obj)
	}
}

func (s *stubClient) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if list == nil {
		return fmt.Errorf("assertion failed: list must not be nil")
	}

	listOptions := (&client.ListOptions{}).ApplyOptions(opts)
	selector := listOptions.LabelSelector

	switch typed := list.(type) {
	case *corev1.PodList:
		items := make([]corev1.Pod, 0, len(s.pods))
		for _, pod := range s.pods {
			if listOptions.Namespace != "" && pod.Namespace != listOptions.Namespace {
				continue
			}
			if selector != nil && !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			items = append(items, *pod.DeepCopy())
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Name == items[j].Name {
				return items[i].Namespace < items[j].Namespace
			}
			return items[i].Name < items[j].Name
		})
		typed.Items = items
		return nil
	case *aggregationv1alpha1.CoderWorkspaceList:
		items := make([]aggregationv1alpha1.CoderWorkspace, 0, len(s.workspaces))
		for _, workspace := range s.workspaces {
			if listOptions.Namespace != "" && workspace.Namespace != listOptions.Namespace {
				continue
			}
			items = append(items, *workspace.DeepCopy())
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Name == items[j].Name {
				return items[i].Namespace < items[j].Namespace
			}
			return items[i].Name < items[j].Name
		})
		typed.Items = items
		return nil
	case *aggregationv1alpha1.CoderTemplateList:
		items := make([]aggregationv1alpha1.CoderTemplate, 0, len(s.templates))
		for _, template := range s.templates {
			if listOptions.Namespace != "" && template.Namespace != listOptions.Namespace {
				continue
			}
			items = append(items, *template.DeepCopy())
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Name == items[j].Name {
				return items[i].Namespace < items[j].Namespace
			}
			return items[i].Name < items[j].Name
		})
		typed.Items = items
		return nil
	default:
		return fmt.Errorf("assertion failed: unsupported list type %T", list)
	}
}

func (s *stubClient) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
	return fmt.Errorf("assertion failed: Apply is not implemented in stub client")
}

func (s *stubClient) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	return fmt.Errorf("assertion failed: Create is not implemented in stub client")
}

func (s *stubClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	return fmt.Errorf("assertion failed: Delete is not implemented in stub client")
}

func (s *stubClient) Update(_ context.Context, obj client.Object, _ ...client.UpdateOption) error {
	if obj == nil {
		return fmt.Errorf("assertion failed: object must not be nil")
	}

	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	switch typed := obj.(type) {
	case *aggregationv1alpha1.CoderWorkspace:
		if _, exists := s.workspaces[key]; !exists {
			return apierrors.NewNotFound(aggregationv1alpha1.Resource("coderworkspaces"), obj.GetName())
		}
		s.workspaces[key] = typed.DeepCopy()
		return nil
	case *aggregationv1alpha1.CoderTemplate:
		if _, exists := s.templates[key]; !exists {
			return apierrors.NewNotFound(aggregationv1alpha1.Resource("codertemplates"), obj.GetName())
		}
		s.templates[key] = typed.DeepCopy()
		return nil
	default:
		return fmt.Errorf("assertion failed: unsupported update type %T", obj)
	}
}

func (s *stubClient) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
	return fmt.Errorf("assertion failed: Patch is not implemented in stub client")
}

func (s *stubClient) DeleteAllOf(_ context.Context, _ client.Object, _ ...client.DeleteAllOfOption) error {
	return fmt.Errorf("assertion failed: DeleteAllOf is not implemented in stub client")
}

func (s *stubClient) Status() client.SubResourceWriter {
	return stubSubResourceClient{}
}

func (s *stubClient) SubResource(_ string) client.SubResourceClient {
	return stubSubResourceClient{}
}

func (s *stubClient) Scheme() *runtime.Scheme {
	return s.scheme
}

func (s *stubClient) RESTMapper() apiMeta.RESTMapper {
	return nil
}

func (s *stubClient) GroupVersionKindFor(_ runtime.Object) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, nil
}

func (s *stubClient) IsObjectNamespaced(_ runtime.Object) (bool, error) {
	return true, nil
}

type stubSubResourceClient struct{}

func (stubSubResourceClient) Get(
	_ context.Context,
	_ client.Object,
	_ client.Object,
	_ ...client.SubResourceGetOption,
) error {
	return fmt.Errorf("assertion failed: subresource Get is not implemented in stub client")
}

func (stubSubResourceClient) Create(
	_ context.Context,
	_ client.Object,
	_ client.Object,
	_ ...client.SubResourceCreateOption,
) error {
	return fmt.Errorf("assertion failed: subresource Create is not implemented in stub client")
}

func (stubSubResourceClient) Update(
	_ context.Context,
	_ client.Object,
	_ ...client.SubResourceUpdateOption,
) error {
	return fmt.Errorf("assertion failed: subresource Update is not implemented in stub client")
}

func (stubSubResourceClient) Patch(
	_ context.Context,
	_ client.Object,
	_ client.Patch,
	_ ...client.SubResourcePatchOption,
) error {
	return fmt.Errorf("assertion failed: subresource Patch is not implemented in stub client")
}

func (stubSubResourceClient) Apply(
	_ context.Context,
	_ runtime.ApplyConfiguration,
	_ ...client.SubResourceApplyOption,
) error {
	return fmt.Errorf("assertion failed: subresource Apply is not implemented in stub client")
}

func newCoreNotFound(resource, name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: resource}, name)
}

func newAppsNotFound(resource, name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: resource}, name)
}

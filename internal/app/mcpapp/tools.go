package mcpapp

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultPodLogTailLines          int64 = 2000
	maxPodLogBytes                  int64 = 1 << 20
	podLogTruncatedSuffix                 = "\n(truncated)"
	defaultEventListLimit           int64 = 200
	maxEventListLimit               int64 = 1000
	defaultControlPlanePodListLimit int64 = 100
	maxControlPlanePodListLimit     int64 = 500
)

type listControlPlanesInput struct {
	Namespace string `json:"namespace,omitempty"`
}

type controlPlaneSummary struct {
	Name          string `json:"name"`
	Namespace     string `json:"namespace"`
	Phase         string `json:"phase"`
	ReadyReplicas int32  `json:"readyReplicas"`
	URL           string `json:"url"`
}

type listControlPlanesOutput struct {
	Items []controlPlaneSummary `json:"items"`
}

type getControlPlaneStatusInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// NOTE: We cannot return metav1.Condition directly because metav1.Time embeds
// time.Time, which causes jsonschema-go schema inference to fail for MCP tool
// schemas.
type controlPlaneConditionSummary struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

type getControlPlaneStatusOutput struct {
	ObservedGeneration int64                          `json:"observedGeneration"`
	ReadyReplicas      int32                          `json:"readyReplicas"`
	URL                string                         `json:"url"`
	Phase              string                         `json:"phase"`
	Conditions         []controlPlaneConditionSummary `json:"conditions"`
}

type listControlPlanePodsInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Limit     int64  `json:"limit,omitempty"`
}

type controlPlanePodSummary struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	Phase           string `json:"phase"`
	NodeName        string `json:"nodeName,omitempty"`
	ReadyContainers int32  `json:"readyContainers"`
	TotalContainers int32  `json:"totalContainers"`
	StartTime       string `json:"startTime,omitempty"`
}

type listControlPlanePodsOutput struct {
	Items     []controlPlanePodSummary `json:"items"`
	Truncated bool                     `json:"truncated,omitempty"`
}

type getControlPlaneDeploymentStatusInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type deploymentConditionSummary struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastUpdateTime     string `json:"lastUpdateTime,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

type getControlPlaneDeploymentStatusOutput struct {
	Name                string                       `json:"name"`
	Namespace           string                       `json:"namespace"`
	Replicas            int32                        `json:"replicas"`
	ReadyReplicas       int32                        `json:"readyReplicas"`
	UpdatedReplicas     int32                        `json:"updatedReplicas"`
	AvailableReplicas   int32                        `json:"availableReplicas"`
	UnavailableReplicas int32                        `json:"unavailableReplicas"`
	ObservedGeneration  int64                        `json:"observedGeneration"`
	Conditions          []deploymentConditionSummary `json:"conditions"`
}

type getServiceStatusInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type servicePortSummary struct {
	Name       string `json:"name,omitempty"`
	Port       int32  `json:"port"`
	TargetPort string `json:"targetPort,omitempty"`
	NodePort   int32  `json:"nodePort,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
}

type getServiceStatusOutput struct {
	Name        string               `json:"name"`
	Namespace   string               `json:"namespace"`
	Type        string               `json:"type"`
	ClusterIP   string               `json:"clusterIP,omitempty"`
	Ports       []servicePortSummary `json:"ports"`
	Annotations map[string]string    `json:"annotations,omitempty"`
}

type listWorkspacesInput struct {
	Namespace string `json:"namespace,omitempty"`
}

type workspaceSummary struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Running      bool   `json:"running"`
	AutoShutdown string `json:"autoShutdown,omitempty"`
}

type listWorkspacesOutput struct {
	Items []workspaceSummary `json:"items"`
}

type getWorkspaceInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type getWorkspaceOutput struct {
	Workspace workspaceSummary `json:"workspace"`
}

type setWorkspaceRunningInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Running   bool   `json:"running"`
}

type setWorkspaceRunningOutput struct {
	Workspace workspaceSummary `json:"workspace"`
	Updated   bool             `json:"updated"`
}

type listTemplatesInput struct {
	Namespace string `json:"namespace,omitempty"`
}

type templateSummary struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Running      bool   `json:"running"`
	AutoShutdown string `json:"autoShutdown,omitempty"`
}

type listTemplatesOutput struct {
	Items []templateSummary `json:"items"`
}

type getTemplateInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type getTemplateOutput struct {
	Template templateSummary `json:"template"`
}

type setTemplateRunningInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Running   bool   `json:"running"`
}

type setTemplateRunningOutput struct {
	Template templateSummary `json:"template"`
	Updated  bool            `json:"updated"`
}

type getEventsInput struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Limit     int64  `json:"limit,omitempty"`
	Continue  string `json:"continue,omitempty"`
}

type eventSummary struct {
	Type          string `json:"type"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	Count         int32  `json:"count"`
	LastTimestamp string `json:"lastTimestamp,omitempty"`
}

type getEventsOutput struct {
	Items    []eventSummary `json:"items"`
	Continue string         `json:"continue,omitempty"`
}

type getPodLogsInput struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	TailLines  *int64 `json:"tailLines,omitempty"`
	LimitBytes *int64 `json:"limitBytes,omitempty"`
	Container  string `json:"container,omitempty"`
}

type getPodLogsOutput struct {
	Logs string `json:"logs"`
}

type checkHealthOutput struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

func registerTools(server *mcp.Server, k8sClient client.Client, clientset kubernetes.Interface) {
	if server == nil {
		panic("assertion failed: MCP server must not be nil")
	}
	if k8sClient == nil {
		panic("assertion failed: Kubernetes client must not be nil")
	}
	if clientset == nil {
		panic("assertion failed: Kubernetes clientset must not be nil")
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_control_planes",
		Description: "List CoderControlPlane resources.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listControlPlanesInput) (*mcp.CallToolResult, listControlPlanesOutput, error) {
		controlPlaneList := &coderv1alpha1.CoderControlPlaneList{}
		listOptions := []client.ListOption{}
		if input.Namespace != "" {
			listOptions = append(listOptions, client.InNamespace(input.Namespace))
		}
		if err := k8sClient.List(ctx, controlPlaneList, listOptions...); err != nil {
			return nil, listControlPlanesOutput{}, fmt.Errorf("list CoderControlPlane resources: %w", err)
		}

		output := listControlPlanesOutput{Items: make([]controlPlaneSummary, 0, len(controlPlaneList.Items))}
		for _, controlPlane := range controlPlaneList.Items {
			output.Items = append(output.Items, controlPlaneSummary{
				Name:          controlPlane.Name,
				Namespace:     controlPlane.Namespace,
				Phase:         controlPlane.Status.Phase,
				ReadyReplicas: controlPlane.Status.ReadyReplicas,
				URL:           controlPlane.Status.URL,
			})
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_control_plane_status",
		Description: "Get status for a CoderControlPlane resource.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getControlPlaneStatusInput) (*mcp.CallToolResult, getControlPlaneStatusOutput, error) {
		if input.Namespace == "" {
			return nil, getControlPlaneStatusOutput{}, fmt.Errorf("namespace is required")
		}
		if input.Name == "" {
			return nil, getControlPlaneStatusOutput{}, fmt.Errorf("name is required")
		}

		controlPlane := &coderv1alpha1.CoderControlPlane{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, controlPlane); err != nil {
			return nil, getControlPlaneStatusOutput{}, fmt.Errorf("get CoderControlPlane %s/%s: %w", input.Namespace, input.Name, err)
		}

		return nil, getControlPlaneStatusOutput{
			ObservedGeneration: controlPlane.Status.ObservedGeneration,
			ReadyReplicas:      controlPlane.Status.ReadyReplicas,
			URL:                controlPlane.Status.URL,
			Phase:              controlPlane.Status.Phase,
			Conditions:         summarizeMetav1Conditions(controlPlane.Status.Conditions),
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_control_plane_pods",
		Description: "List pods for a CoderControlPlane by namespace and name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listControlPlanePodsInput) (*mcp.CallToolResult, listControlPlanePodsOutput, error) {
		output, err := listControlPlanePods(ctx, k8sClient, input)
		if err != nil {
			return nil, listControlPlanePodsOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_control_plane_deployment_status",
		Description: "Get Deployment status for a CoderControlPlane by namespace and name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getControlPlaneDeploymentStatusInput) (*mcp.CallToolResult, getControlPlaneDeploymentStatusOutput, error) {
		output, err := getControlPlaneDeploymentStatus(ctx, k8sClient, input)
		if err != nil {
			return nil, getControlPlaneDeploymentStatusOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_service_status",
		Description: "Get Kubernetes Service status by namespace and name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getServiceStatusInput) (*mcp.CallToolResult, getServiceStatusOutput, error) {
		output, err := getServiceStatus(ctx, k8sClient, input)
		if err != nil {
			return nil, getServiceStatusOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_workspaces",
		Description: "List CoderWorkspace resources.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listWorkspacesInput) (*mcp.CallToolResult, listWorkspacesOutput, error) {
		workspaceList := &aggregationv1alpha1.CoderWorkspaceList{}
		listOptions := []client.ListOption{}
		if input.Namespace != "" {
			listOptions = append(listOptions, client.InNamespace(input.Namespace))
		}
		if err := k8sClient.List(ctx, workspaceList, listOptions...); err != nil {
			return nil, listWorkspacesOutput{}, fmt.Errorf("list CoderWorkspace resources: %w", err)
		}

		output := listWorkspacesOutput{Items: make([]workspaceSummary, 0, len(workspaceList.Items))}
		for _, workspace := range workspaceList.Items {
			output.Items = append(output.Items, workspaceToSummary(&workspace))
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_workspace",
		Description: "Get a CoderWorkspace resource by namespace and name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, getWorkspaceOutput, error) {
		workspace, err := getWorkspaceDetail(ctx, k8sClient, input)
		if err != nil {
			return nil, getWorkspaceOutput{}, err
		}
		return nil, getWorkspaceOutput{Workspace: workspace}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_workspace_running",
		Description: "Set spec.running for a CoderWorkspace resource.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input setWorkspaceRunningInput) (*mcp.CallToolResult, setWorkspaceRunningOutput, error) {
		output, err := setWorkspaceRunning(ctx, k8sClient, input)
		if err != nil {
			return nil, setWorkspaceRunningOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_templates",
		Description: "List CoderTemplate resources.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listTemplatesInput) (*mcp.CallToolResult, listTemplatesOutput, error) {
		templateList := &aggregationv1alpha1.CoderTemplateList{}
		listOptions := []client.ListOption{}
		if input.Namespace != "" {
			listOptions = append(listOptions, client.InNamespace(input.Namespace))
		}
		if err := k8sClient.List(ctx, templateList, listOptions...); err != nil {
			return nil, listTemplatesOutput{}, fmt.Errorf("list CoderTemplate resources: %w", err)
		}

		output := listTemplatesOutput{Items: make([]templateSummary, 0, len(templateList.Items))}
		for _, template := range templateList.Items {
			output.Items = append(output.Items, templateToSummary(&template))
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_template",
		Description: "Get a CoderTemplate resource by namespace and name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getTemplateInput) (*mcp.CallToolResult, getTemplateOutput, error) {
		template, err := getTemplateDetail(ctx, k8sClient, input)
		if err != nil {
			return nil, getTemplateOutput{}, err
		}
		return nil, getTemplateOutput{Template: template}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_template_running",
		Description: "Set spec.running for a CoderTemplate resource.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input setTemplateRunningInput) (*mcp.CallToolResult, setTemplateRunningOutput, error) {
		output, err := setTemplateRunning(ctx, k8sClient, input)
		if err != nil {
			return nil, setTemplateRunningOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_events",
		Description: "Get Kubernetes events for an object.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getEventsInput) (*mcp.CallToolResult, getEventsOutput, error) {
		if input.Namespace == "" {
			return nil, getEventsOutput{}, fmt.Errorf("namespace is required")
		}

		selector := fields.Set{}
		if input.Name != "" {
			selector["involvedObject.name"] = input.Name
		}
		if input.Kind != "" {
			selector["involvedObject.kind"] = input.Kind
		}

		limit := input.Limit
		if limit <= 0 {
			limit = defaultEventListLimit
		}
		if limit > maxEventListLimit {
			limit = maxEventListLimit
		}

		listOptions := metav1.ListOptions{
			Limit:    limit,
			Continue: input.Continue,
		}
		if len(selector) > 0 {
			listOptions.FieldSelector = selector.String()
		}

		eventList, err := clientset.CoreV1().Events(input.Namespace).List(ctx, listOptions)
		if err != nil {
			return nil, getEventsOutput{}, fmt.Errorf("list events in namespace %q: %w", input.Namespace, err)
		}

		output := getEventsOutput{
			Items:    make([]eventSummary, 0, len(eventList.Items)),
			Continue: eventList.Continue,
		}
		for _, event := range eventList.Items {
			output.Items = append(output.Items, eventSummary{
				Type:          event.Type,
				Reason:        event.Reason,
				Message:       event.Message,
				Count:         event.Count,
				LastTimestamp: eventTimestamp(event),
			})
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_pod_logs",
		Description: "Get logs from a pod.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getPodLogsInput) (*mcp.CallToolResult, getPodLogsOutput, error) {
		if input.Namespace == "" {
			return nil, getPodLogsOutput{}, fmt.Errorf("namespace is required")
		}
		if input.Name == "" {
			return nil, getPodLogsOutput{}, fmt.Errorf("name is required")
		}

		tailLines := input.TailLines
		if tailLines == nil {
			defaultTailLines := defaultPodLogTailLines
			tailLines = &defaultTailLines
		}

		limitBytes := maxPodLogBytes
		if input.LimitBytes != nil && *input.LimitBytes > 0 && *input.LimitBytes < limitBytes {
			limitBytes = *input.LimitBytes
		}

		logOptions := &corev1.PodLogOptions{
			Container: input.Container,
			TailLines: tailLines,
		}
		stream, err := clientset.CoreV1().Pods(input.Namespace).GetLogs(input.Name, logOptions).Stream(ctx)
		if err != nil {
			return nil, getPodLogsOutput{}, fmt.Errorf("stream logs for pod %s/%s: %w", input.Namespace, input.Name, err)
		}
		defer func() {
			_ = stream.Close()
		}()

		logs, err := io.ReadAll(io.LimitReader(stream, limitBytes+1))
		if err != nil {
			return nil, getPodLogsOutput{}, fmt.Errorf("read logs for pod %s/%s: %w", input.Namespace, input.Name, err)
		}

		truncated := int64(len(logs)) > limitBytes
		if truncated {
			logs = logs[:limitBytes]
		}

		outputLogs := string(logs)
		if truncated {
			outputLogs += podLogTruncatedSuffix
		}

		return nil, getPodLogsOutput{Logs: outputLogs}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_health",
		Description: "Check MCP server health and Kubernetes API connectivity.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, checkHealthOutput, error) {
		if _, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			return nil, checkHealthOutput{}, fmt.Errorf("list namespaces for connectivity check: %w", err)
		}
		return nil, checkHealthOutput{
			Status:  "ok",
			Version: serverImplementationVersion,
		}, nil
	})
}

func listControlPlanePods(ctx context.Context, k8sClient client.Client, input listControlPlanePodsInput) (listControlPlanePodsOutput, error) {
	if k8sClient == nil {
		return listControlPlanePodsOutput{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return listControlPlanePodsOutput{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return listControlPlanePodsOutput{}, fmt.Errorf("name is required")
	}

	labels := controlPlaneWorkloadLabels(input.Name)
	if len(labels) == 0 {
		return listControlPlanePodsOutput{}, fmt.Errorf("assertion failed: control plane workload labels must not be empty")
	}

	podList := &corev1.PodList{}
	if err := k8sClient.List(ctx, podList, client.InNamespace(input.Namespace), client.MatchingLabels(labels)); err != nil {
		return listControlPlanePodsOutput{}, fmt.Errorf("list control plane pods for %s/%s: %w", input.Namespace, input.Name, err)
	}

	sort.Slice(podList.Items, func(i, j int) bool {
		if podList.Items[i].Name == podList.Items[j].Name {
			return podList.Items[i].Namespace < podList.Items[j].Namespace
		}
		return podList.Items[i].Name < podList.Items[j].Name
	})

	limit := sanitizeControlPlanePodListLimit(input.Limit)
	if limit <= 0 {
		return listControlPlanePodsOutput{}, fmt.Errorf("assertion failed: pod list limit must be positive")
	}

	output := listControlPlanePodsOutput{Items: make([]controlPlanePodSummary, 0, minInt(len(podList.Items), int(limit)))}
	for i, pod := range podList.Items {
		if int64(i) >= limit {
			output.Truncated = true
			break
		}

		readyContainers := int32(0)
		for _, status := range pod.Status.ContainerStatuses {
			if status.Ready {
				readyContainers++
			}
		}

		totalContainers, err := safeInt32Count(len(pod.Spec.Containers))
		if err != nil {
			return listControlPlanePodsOutput{}, err
		}
		if totalContainers == 0 {
			totalContainers, err = safeInt32Count(len(pod.Status.ContainerStatuses))
			if err != nil {
				return listControlPlanePodsOutput{}, err
			}
		}

		output.Items = append(output.Items, controlPlanePodSummary{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			Phase:           string(pod.Status.Phase),
			NodeName:        pod.Spec.NodeName,
			ReadyContainers: readyContainers,
			TotalContainers: totalContainers,
			StartTime:       formatOptionalTime(pod.Status.StartTime),
		})
	}

	return output, nil
}

func getControlPlaneDeploymentStatus(
	ctx context.Context,
	k8sClient client.Client,
	input getControlPlaneDeploymentStatusInput,
) (getControlPlaneDeploymentStatusOutput, error) {
	if k8sClient == nil {
		return getControlPlaneDeploymentStatusOutput{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return getControlPlaneDeploymentStatusOutput{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return getControlPlaneDeploymentStatusOutput{}, fmt.Errorf("name is required")
	}

	deployment := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, deployment); err != nil {
		return getControlPlaneDeploymentStatusOutput{}, fmt.Errorf("get deployment %s/%s: %w", input.Namespace, input.Name, err)
	}

	conditions := make([]deploymentConditionSummary, 0, len(deployment.Status.Conditions))
	for _, condition := range deployment.Status.Conditions {
		conditions = append(conditions, deploymentConditionSummary{
			Type:               string(condition.Type),
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastUpdateTime:     formatTime(condition.LastUpdateTime),
			LastTransitionTime: formatTime(condition.LastTransitionTime),
		})
	}
	sort.Slice(conditions, func(i, j int) bool {
		return conditions[i].Type < conditions[j].Type
	})

	return getControlPlaneDeploymentStatusOutput{
		Name:                deployment.Name,
		Namespace:           deployment.Namespace,
		Replicas:            desiredReplicas(deployment.Spec.Replicas),
		ReadyReplicas:       deployment.Status.ReadyReplicas,
		UpdatedReplicas:     deployment.Status.UpdatedReplicas,
		AvailableReplicas:   deployment.Status.AvailableReplicas,
		UnavailableReplicas: deployment.Status.UnavailableReplicas,
		ObservedGeneration:  deployment.Status.ObservedGeneration,
		Conditions:          conditions,
	}, nil
}

func getServiceStatus(ctx context.Context, k8sClient client.Client, input getServiceStatusInput) (getServiceStatusOutput, error) {
	if k8sClient == nil {
		return getServiceStatusOutput{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return getServiceStatusOutput{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return getServiceStatusOutput{}, fmt.Errorf("name is required")
	}

	service := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, service); err != nil {
		return getServiceStatusOutput{}, fmt.Errorf("get service %s/%s: %w", input.Namespace, input.Name, err)
	}

	ports := make([]servicePortSummary, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		ports = append(ports, servicePortSummary{
			Name:       port.Name,
			Port:       port.Port,
			TargetPort: port.TargetPort.String(),
			NodePort:   port.NodePort,
			Protocol:   string(port.Protocol),
		})
	}

	return getServiceStatusOutput{
		Name:        service.Name,
		Namespace:   service.Namespace,
		Type:        string(service.Spec.Type),
		ClusterIP:   service.Spec.ClusterIP,
		Ports:       ports,
		Annotations: cloneStringMap(service.Annotations),
	}, nil
}

func getWorkspaceDetail(ctx context.Context, k8sClient client.Client, input getWorkspaceInput) (workspaceSummary, error) {
	if k8sClient == nil {
		return workspaceSummary{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return workspaceSummary{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return workspaceSummary{}, fmt.Errorf("name is required")
	}

	workspace := &aggregationv1alpha1.CoderWorkspace{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, workspace); err != nil {
		return workspaceSummary{}, fmt.Errorf("get CoderWorkspace %s/%s: %w", input.Namespace, input.Name, err)
	}
	if workspace.Namespace != input.Namespace || workspace.Name != input.Name {
		return workspaceSummary{}, fmt.Errorf(
			"assertion failed: fetched workspace %s/%s does not match request %s/%s",
			workspace.Namespace,
			workspace.Name,
			input.Namespace,
			input.Name,
		)
	}

	return workspaceToSummary(workspace), nil
}

func setWorkspaceRunning(
	ctx context.Context,
	k8sClient client.Client,
	input setWorkspaceRunningInput,
) (setWorkspaceRunningOutput, error) {
	if k8sClient == nil {
		return setWorkspaceRunningOutput{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return setWorkspaceRunningOutput{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return setWorkspaceRunningOutput{}, fmt.Errorf("name is required")
	}

	workspace := &aggregationv1alpha1.CoderWorkspace{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, workspace); err != nil {
		return setWorkspaceRunningOutput{}, fmt.Errorf("get CoderWorkspace %s/%s: %w", input.Namespace, input.Name, err)
	}
	if workspace.Namespace != input.Namespace || workspace.Name != input.Name {
		return setWorkspaceRunningOutput{}, fmt.Errorf(
			"assertion failed: fetched workspace %s/%s does not match request %s/%s",
			workspace.Namespace,
			workspace.Name,
			input.Namespace,
			input.Name,
		)
	}

	updated := workspace.Spec.Running != input.Running
	workspace.Spec.Running = input.Running
	if updated {
		if err := k8sClient.Update(ctx, workspace); err != nil {
			return setWorkspaceRunningOutput{}, fmt.Errorf("update CoderWorkspace %s/%s: %w", input.Namespace, input.Name, err)
		}
	}

	return setWorkspaceRunningOutput{
		Workspace: workspaceToSummary(workspace),
		Updated:   updated,
	}, nil
}

func getTemplateDetail(ctx context.Context, k8sClient client.Client, input getTemplateInput) (templateSummary, error) {
	if k8sClient == nil {
		return templateSummary{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return templateSummary{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return templateSummary{}, fmt.Errorf("name is required")
	}

	template := &aggregationv1alpha1.CoderTemplate{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, template); err != nil {
		return templateSummary{}, fmt.Errorf("get CoderTemplate %s/%s: %w", input.Namespace, input.Name, err)
	}
	if template.Namespace != input.Namespace || template.Name != input.Name {
		return templateSummary{}, fmt.Errorf(
			"assertion failed: fetched template %s/%s does not match request %s/%s",
			template.Namespace,
			template.Name,
			input.Namespace,
			input.Name,
		)
	}

	return templateToSummary(template), nil
}

func setTemplateRunning(
	ctx context.Context,
	k8sClient client.Client,
	input setTemplateRunningInput,
) (setTemplateRunningOutput, error) {
	if k8sClient == nil {
		return setTemplateRunningOutput{}, fmt.Errorf("assertion failed: Kubernetes client must not be nil")
	}
	if input.Namespace == "" {
		return setTemplateRunningOutput{}, fmt.Errorf("namespace is required")
	}
	if input.Name == "" {
		return setTemplateRunningOutput{}, fmt.Errorf("name is required")
	}

	template := &aggregationv1alpha1.CoderTemplate{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.Name}, template); err != nil {
		return setTemplateRunningOutput{}, fmt.Errorf("get CoderTemplate %s/%s: %w", input.Namespace, input.Name, err)
	}
	if template.Namespace != input.Namespace || template.Name != input.Name {
		return setTemplateRunningOutput{}, fmt.Errorf(
			"assertion failed: fetched template %s/%s does not match request %s/%s",
			template.Namespace,
			template.Name,
			input.Namespace,
			input.Name,
		)
	}

	updated := template.Spec.Running != input.Running
	template.Spec.Running = input.Running
	if updated {
		if err := k8sClient.Update(ctx, template); err != nil {
			return setTemplateRunningOutput{}, fmt.Errorf("update CoderTemplate %s/%s: %w", input.Namespace, input.Name, err)
		}
	}

	return setTemplateRunningOutput{
		Template: templateToSummary(template),
		Updated:  updated,
	}, nil
}

func controlPlaneWorkloadLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "coder-control-plane",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "coder-k8s",
	}
}

func workspaceToSummary(workspace *aggregationv1alpha1.CoderWorkspace) workspaceSummary {
	if workspace == nil {
		panic("assertion failed: workspace must not be nil")
	}

	return workspaceSummary{
		Name:         workspace.Name,
		Namespace:    workspace.Namespace,
		Running:      workspace.Spec.Running,
		AutoShutdown: formatOptionalTime(workspace.Status.AutoShutdown),
	}
}

func templateToSummary(template *aggregationv1alpha1.CoderTemplate) templateSummary {
	if template == nil {
		panic("assertion failed: template must not be nil")
	}

	return templateSummary{
		Name:         template.Name,
		Namespace:    template.Namespace,
		Running:      template.Spec.Running,
		AutoShutdown: formatOptionalTime(template.Status.AutoShutdown),
	}
}

func desiredReplicas(replicas *int32) int32 {
	if replicas == nil {
		return 0
	}
	return *replicas
}

func summarizeMetav1Conditions(in []metav1.Condition) []controlPlaneConditionSummary {
	if len(in) == 0 {
		return nil
	}

	out := make([]controlPlaneConditionSummary, 0, len(in))
	for _, cond := range in {
		summary := controlPlaneConditionSummary{
			Type:               cond.Type,
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			ObservedGeneration: cond.ObservedGeneration,
		}
		if !cond.LastTransitionTime.IsZero() {
			summary.LastTransitionTime = cond.LastTransitionTime.Time.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, summary)
	}

	return out
}

func sanitizeControlPlanePodListLimit(limit int64) int64 {
	if limit <= 0 {
		return defaultControlPlanePodListLimit
	}
	if limit > maxControlPlanePodListLimit {
		return maxControlPlanePodListLimit
	}
	return limit
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func safeInt32Count(value int) (int32, error) {
	if value < 0 {
		return 0, fmt.Errorf("assertion failed: count must not be negative")
	}
	if value > math.MaxInt32 {
		return 0, fmt.Errorf("assertion failed: count %d exceeds int32 max %d", value, math.MaxInt32)
	}
	return int32(value), nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}

	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func formatOptionalTime(value *metav1.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func formatTime(value metav1.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func eventTimestamp(event corev1.Event) string {
	if !event.EventTime.IsZero() {
		return event.EventTime.Time.UTC().Format(time.RFC3339)
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time.UTC().Format(time.RFC3339)
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time.UTC().Format(time.RFC3339)
	}
	return ""
}

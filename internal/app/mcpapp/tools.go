package mcpapp

import (
	"context"
	"fmt"
	"io"
	"time"

	aggregationv1alpha1 "github.com/coder/coder-k8s/api/aggregation/v1alpha1"
	coderv1alpha1 "github.com/coder/coder-k8s/api/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultPodLogTailLines int64 = 2000
	maxPodLogBytes         int64 = 1 << 20
	podLogTruncatedSuffix        = "\n(truncated)"
	defaultEventListLimit  int64 = 200
	maxEventListLimit      int64 = 1000
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

type getControlPlaneStatusOutput struct {
	ObservedGeneration int64              `json:"observedGeneration"`
	ReadyReplicas      int32              `json:"readyReplicas"`
	URL                string             `json:"url"`
	Phase              string             `json:"phase"`
	Conditions         []metav1.Condition `json:"conditions"`
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
			Conditions:         controlPlane.Status.Conditions,
		}, nil
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
			output.Items = append(output.Items, workspaceSummary{
				Name:         workspace.Name,
				Namespace:    workspace.Namespace,
				Running:      workspace.Spec.Running,
				AutoShutdown: formatOptionalTime(workspace.Status.AutoShutdown),
			})
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
			output.Items = append(output.Items, templateSummary{
				Name:         template.Name,
				Namespace:    template.Namespace,
				Running:      template.Spec.Running,
				AutoShutdown: formatOptionalTime(template.Status.AutoShutdown),
			})
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

func formatOptionalTime(value *metav1.Time) string {
	if value == nil || value.IsZero() {
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

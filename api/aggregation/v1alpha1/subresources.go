package v1alpha1

import (
	"fmt"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
)

// CoderWorkspaceLogOptions are the query options of the coderworkspaces/log subresource.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CoderWorkspaceLogOptions struct {
	metav1.TypeMeta `json:",inline"`

	// LimitBytes ends the response after this many bytes. It may cut a line.
	LimitBytes *int64 `json:"limitBytes,omitempty"`

	// Follow keeps the response open and streams new entries until the build ends.
	Follow bool `json:"follow,omitempty"`
}

// CoderTemplateVersionPromotionSpec names the template version to activate.
type CoderTemplateVersionPromotionSpec struct {
	// VersionID is the Coder ID (a UUID) of the template version to activate. It must be a version of
	// the template named in the request path.
	VersionID string `json:"versionID"`
}

// CoderTemplateVersionPromotionResult is the outcome of a promotion request.
type CoderTemplateVersionPromotionResult string

const (
	// PromotionResultPromoted means that this request changed the active version.
	PromotionResultPromoted CoderTemplateVersionPromotionResult = "Promoted"
	// PromotionResultAlreadyActive means that the version was already active, so nothing changed.
	PromotionResultAlreadyActive CoderTemplateVersionPromotionResult = "AlreadyActive"
	// PromotionResultWouldPromote means that a dry-run request found that a real request would
	// change the active version. Nothing changed.
	PromotionResultWouldPromote CoderTemplateVersionPromotionResult = "WouldPromote"
)

// CoderTemplateVersionPromotionStatus reports the observed outcome of a promotion request.
type CoderTemplateVersionPromotionStatus struct {
	// Result is Promoted, AlreadyActive or WouldPromote.
	Result CoderTemplateVersionPromotionResult `json:"result,omitempty"`
	// PreviousActiveVersionID is the active version that the request observed before it acted.
	PreviousActiveVersionID string `json:"previousActiveVersionID,omitempty"`
	// ActiveVersionID is the active version that the request observed after it acted.
	ActiveVersionID string `json:"activeVersionID,omitempty"`
}

// CoderTemplateVersionPromotion is the request and response body of the codertemplates/promote
// subresource. It activates one version of a template; rollback is a promotion of an older version.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type CoderTemplateVersionPromotion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CoderTemplateVersionPromotionSpec   `json:"spec"`
	Status CoderTemplateVersionPromotionStatus `json:"status,omitempty"`
}

// convertURLValuesToCoderWorkspaceLogOptions decodes log query parameters. Unknown parameters are
// ignored, as for Pod logs. A malformed number is an error, which the API server answers with 400.
func convertURLValuesToCoderWorkspaceLogOptions(in *url.Values, out *CoderWorkspaceLogOptions, s conversion.Scope) error {
	if in == nil || out == nil {
		return fmt.Errorf("assertion failed: log options conversion needs non-nil input and output")
	}
	*out = CoderWorkspaceLogOptions{TypeMeta: out.TypeMeta}
	if values, ok := (*in)["limitBytes"]; ok {
		if err := runtime.Convert_Slice_string_To_Pointer_int64(&values, &out.LimitBytes, s); err != nil {
			return fmt.Errorf("limitBytes: %w", err)
		}
	}
	if values, ok := (*in)["follow"]; ok {
		// As for Pod logs, a bare "?follow" means true.
		if err := runtime.Convert_Slice_string_To_bool(&values, &out.Follow, s); err != nil {
			return fmt.Errorf("follow: %w", err)
		}
	}
	return nil
}

func addSubresourceConversionFuncs(scheme *runtime.Scheme) error {
	return scheme.AddConversionFunc((*url.Values)(nil), (*CoderWorkspaceLogOptions)(nil), func(a, b interface{}, s conversion.Scope) error {
		return convertURLValuesToCoderWorkspaceLogOptions(a.(*url.Values), b.(*CoderWorkspaceLogOptions), s)
	})
}

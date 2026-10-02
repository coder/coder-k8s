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
	return nil
}

func addSubresourceConversionFuncs(scheme *runtime.Scheme) error {
	return scheme.AddConversionFunc((*url.Values)(nil), (*CoderWorkspaceLogOptions)(nil), func(a, b interface{}, s conversion.Scope) error {
		return convertURLValuesToCoderWorkspaceLogOptions(a.(*url.Values), b.(*CoderWorkspaceLogOptions), s)
	})
}

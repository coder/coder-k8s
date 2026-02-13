package storage

import (
	"fmt"

	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
)

// watchBroadcasterQueueLen is the default queue length for watch broadcasters.
const watchBroadcasterQueueLen = 100

// supportedWatchFieldSelectors lists the metadata fields supported for watch field selectors.
var supportedWatchFieldSelectors = map[string]bool{
	"metadata.name":      true,
	"metadata.namespace": true,
}

// validateFieldSelector checks that all field selector requirements use supported fields.
func validateFieldSelector(sel fields.Selector) error {
	if sel == nil || sel.Empty() {
		return nil
	}

	reqs := sel.Requirements()
	for _, req := range reqs {
		if !supportedWatchFieldSelectors[req.Field] {
			return fmt.Errorf(
				"field selector %q is not supported; supported fields: metadata.name, metadata.namespace",
				req.Field,
			)
		}
	}

	return nil
}

// filterForListOptions builds a watch.FilterFunc that applies namespace, label, and field selector filtering.
// Returns nil if no filtering is needed.
func filterForListOptions(requestNamespace string, opts *metainternalversion.ListOptions) (watch.FilterFunc, error) {
	var labelSel labels.Selector
	var fieldSel fields.Selector

	if opts != nil {
		if opts.LabelSelector != nil && !opts.LabelSelector.Empty() {
			labelSel = opts.LabelSelector
		}
		if opts.FieldSelector != nil && !opts.FieldSelector.Empty() {
			if err := validateFieldSelector(opts.FieldSelector); err != nil {
				return nil, err
			}
			fieldSel = opts.FieldSelector
		}
	}

	if requestNamespace == "" && labelSel == nil && fieldSel == nil {
		return nil, nil
	}

	return func(in watch.Event) (watch.Event, bool) {
		obj, ok := in.Object.(metav1.ObjectMetaAccessor)
		if !ok {
			return in, true
		}

		meta := obj.GetObjectMeta()
		if requestNamespace != "" && meta.GetNamespace() != requestNamespace {
			return in, false
		}

		if labelSel != nil && !labelSel.Matches(labels.Set(meta.GetLabels())) {
			return in, false
		}

		if fieldSel != nil {
			fieldSet := fields.Set{
				"metadata.name":      meta.GetName(),
				"metadata.namespace": meta.GetNamespace(),
			}
			if !fieldSel.Matches(fieldSet) {
				return in, false
			}
		}

		return in, true
	}, nil
}

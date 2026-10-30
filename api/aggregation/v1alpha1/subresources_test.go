package v1alpha1

import (
	"net/url"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestCoderWorkspaceLogOptionsQueryDecoding(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	codec := runtime.NewParameterCodec(scheme)

	var opts CoderWorkspaceLogOptions
	if err := codec.DecodeParameters(url.Values{"limitBytes": {"64"}, "tailLines": {"5"}, "unknown": {"x"}}, SchemeGroupVersion, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.LimitBytes == nil || *opts.LimitBytes != 64 {
		t.Fatalf("limitBytes: %v", opts.LimitBytes)
	}
	if opts.TailLines == nil || *opts.TailLines != 5 {
		t.Fatalf("tailLines: %v", opts.TailLines)
	}
	for value, want := range map[string]bool{"": true, "true": true, "false": false} {
		var follow CoderWorkspaceLogOptions
		if err := codec.DecodeParameters(url.Values{"follow": {value}}, SchemeGroupVersion, &follow); err != nil || follow.Follow != want {
			t.Fatalf("follow=%q: err=%v follow=%v", value, err, follow.Follow)
		}
	}
	var empty CoderWorkspaceLogOptions
	if err := codec.DecodeParameters(url.Values{}, SchemeGroupVersion, &empty); err != nil || empty.LimitBytes != nil || empty.TailLines != nil {
		t.Fatalf("no parameters: err=%v limitBytes=%v tailLines=%v", err, empty.LimitBytes, empty.TailLines)
	}
	for _, name := range []string{"limitBytes", "tailLines"} {
		if err := codec.DecodeParameters(url.Values{name: {"abc"}}, SchemeGroupVersion, &empty); err == nil {
			t.Fatalf("a malformed %s must fail to decode", name)
		}
	}
}

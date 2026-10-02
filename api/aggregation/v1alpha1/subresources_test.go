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
	if err := codec.DecodeParameters(url.Values{"limitBytes": {"64"}, "unknown": {"x"}}, SchemeGroupVersion, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.LimitBytes == nil || *opts.LimitBytes != 64 {
		t.Fatalf("limitBytes: %v", opts.LimitBytes)
	}
	var empty CoderWorkspaceLogOptions
	if err := codec.DecodeParameters(url.Values{}, SchemeGroupVersion, &empty); err != nil || empty.LimitBytes != nil {
		t.Fatalf("no parameters: err=%v limitBytes=%v", err, empty.LimitBytes)
	}
	if err := codec.DecodeParameters(url.Values{"limitBytes": {"abc"}}, SchemeGroupVersion, &empty); err == nil {
		t.Fatal("a malformed limitBytes must fail to decode")
	}
}

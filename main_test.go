package main

import "testing"

func TestHelloMessage(t *testing.T) {
	t.Helper()
	if got, want := helloMessage(), "hello world"; got != want {
		t.Fatalf("unexpected hello message: got %q want %q", got, want)
	}
}

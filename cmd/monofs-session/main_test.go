package main

import (
	"testing"
	"time"
)

func TestSocketTimeoutForAction(t *testing.T) {
	t.Parallel()

	tests := map[string]time.Duration{
		"status":     defaultSocketTimeout,
		"push":       pushSocketTimeout,
		"push-blobs": pushSocketTimeout,
		"commit":     defaultSocketTimeout,
		"":           defaultSocketTimeout,
	}

	for action, want := range tests {
		if got := socketTimeoutForAction(action); got != want {
			t.Fatalf("socketTimeoutForAction(%q) = %v, want %v", action, got, want)
		}
	}
}

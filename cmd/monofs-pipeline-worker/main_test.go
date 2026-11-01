package main

import (
	"net/http/httptest"
	"testing"
)

func TestExecutorRequestAuthorized(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		header string
		value  string
		want   bool
	}{
		{name: "bearer", token: "secret", header: "Authorization", value: "Bearer secret", want: true},
		{name: "dedicated header", token: "secret", header: "X-Monofs-Executor-Token", value: "secret", want: true},
		{name: "wrong token", token: "secret", header: "Authorization", value: "Bearer nope", want: false},
		{name: "missing token", token: "secret", want: false},
		{name: "no configured token", token: "", header: "Authorization", value: "Bearer secret", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/execute", nil)
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			if got := executorRequestAuthorized(req, tt.token); got != tt.want {
				t.Fatalf("executorRequestAuthorized() = %v, want %v", got, tt.want)
			}
		})
	}
}

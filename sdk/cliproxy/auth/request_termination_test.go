package auth

import (
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestRequestTerminatedError(t *testing.T) {
	errTerminated := &cliproxyexecutor.RequestTerminatedError{HTTPStatus: http.StatusTooManyRequests}
	if !isRequestTerminatedError(errTerminated) {
		t.Fatal("isRequestTerminatedError() = false")
	}
}

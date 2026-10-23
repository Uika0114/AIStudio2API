package app

import (
	"errors"
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"net/url"
	"testing"
)

func TestRemoteFailureOutranksOuterWorkerFailedBranch(t *testing.T) {
	cause := errors.Join(aistudio.SessionError("HELPER_UNAVAILABLE"), &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: errors.New("fixture transport")})
	for _, started := range []bool{false, true} {
		if generationAttemptRetryable(t.Context(), cause, true, started) {
			t.Fatal("outer WorkerFailed OR overrode remote stop marker")
		}
	}
	if !generationAttemptRetryable(t.Context(), errors.New("fixture worker failure"), true, false) {
		t.Fatal("ordinary worker recovery changed")
	}
}

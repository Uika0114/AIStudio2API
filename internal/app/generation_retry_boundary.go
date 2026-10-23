package app

import (
	"context"
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func generationAttemptRetryable(ctx context.Context, err error, localWorkerFailure, semanticStarted bool) bool {
	if aistudio.IsSessionFailure(err) {
		return false
	}
	return (retryableGenerateAccountError(ctx, err) || localWorkerFailure) && !semanticStarted
}

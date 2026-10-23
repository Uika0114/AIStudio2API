package authhelper

import (
	"context"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// A NOT_STARTED receipt is emitted only after the authority durably records
// that this exact operation performed no authentication. Client timeout alone
// can never clear a pending operation.
type notStartedError struct{ receipt RefreshRequest }

func (e *notStartedError) Error() string { return "remote authentication: NOT_STARTED" }
func (e *notStartedError) Unwrap() error { return failure("NOT_STARTED") }
func operationFailure(o *aistudio.SessionOperation, r RefreshRequest) error {
	if o.Failure == "NOT_STARTED" {
		return &notStartedError{receipt: r}
	}
	return failure(o.Failure)
}
func (b *Broker) expiredUnstarted(state aistudio.StorageState, meta *aistudio.SessionState, m Mapping, r RefreshRequest) error {
	a := meta.Authority
	if a.Operation != nil {
		a.History = append(a.History, *a.Operation)
	}
	a.Operation = &aistudio.SessionOperation{ID: r.OperationID, Reason: r.Reason, Epoch: r.Epoch, BaseCookie: r.BaseCookie, BaseMaterial: r.BaseMaterial, Deadline: r.Deadline, Stage: "failed", Failure: "NOT_STARTED", HTTP: &aistudio.AuthHTTPCounts{}}
	ctx, cancel := context.WithTimeout(b.lifecycle, 5*time.Second)
	defer cancel()
	if state.SetSession(meta) != nil || b.write(ctx, m.StateFile, &state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return &notStartedError{receipt: r}
}
func (b *Broker) historyResult(state aistudio.StorageState, meta *aistudio.SessionState, m Mapping, r RefreshRequest) (*RefreshResponse, error, bool) {
	for i := range meta.Authority.History {
		o := &meta.Authority.History[i]
		if o.ID != r.OperationID {
			continue
		}
		if !operationMatches(o, r) {
			return nil, failure("OPERATION_CONFLICT"), true
		}
		if o.Stage == "failed" {
			return nil, operationFailure(o, r), true
		}
		if o.Stage != "acked" && o.Stage != "ready" {
			return nil, failure("INDETERMINATE"), true
		}
		if o.ResultCookie != meta.Authority.CookieRevision || o.ResultMaterial != meta.Authority.MaterialVersion {
			return nil, failure("RESULT_SUPERSEDED"), true
		}
		copyMeta := *meta
		copyAuthority := *meta.Authority
		copyAuthority.Operation = o
		copyMeta.Authority = &copyAuthority
		result, err := resultFromState(state, &copyMeta, m, r)
		return result, err, true
	}
	return nil, nil, false
}

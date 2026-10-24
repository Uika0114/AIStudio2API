package aistudio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// AntiObservationVersion versions only the body-free observation format.
const AntiObservationVersion = 1
const antiBehaviorVersion = "unified-reply-v2"

// AntiTruncationObservation is a cumulative snapshot of one transport lifetime.
// It contains no tool names, arguments, reply text, signatures or error strings.
type AntiTruncationObservation struct {
	Version        int                    `json:"version"`
	ID             string                 `json:"id"`
	Revision       uint64                 `json:"revision"`
	RequestID      string                 `json:"request_id"`
	ParentID       string                 `json:"parent_id,omitempty"`
	Purpose        string                 `json:"purpose"`
	Behavior       string                 `json:"behavior"`
	SystemPrompt   bool                   `json:"system_prompt"`
	Injected       bool                   `json:"injected"`
	Sent           bool                   `json:"sent"`
	Complete       bool                   `json:"complete"`
	End            string                 `json:"end"`
	RequestedModel string                 `json:"requested_model"`
	BaseModel      string                 `json:"base_model"`
	Channel        string                 `json:"channel,omitempty"`
	Rounds         []AntiRoundObservation `json:"rounds"`
}

// AntiRoundObservation belongs to one physical send, never a cumulative round.
type AntiRoundObservation struct {
	AttemptID    string `json:"attempt_id"`
	Round        int    `json:"round"`
	Sent         bool   `json:"sent"`
	Response     bool   `json:"response"`
	Status       int    `json:"status,omitempty"`
	Calls        int    `json:"calls"`
	Legal        int    `json:"legal"`
	Empty        int    `json:"empty"`
	Invalid      int    `json:"invalid"`
	Rejected     int    `json:"rejected"`
	RejectReason string `json:"reject_reason,omitempty"`
	RealTools    int    `json:"real_tools"`
	OrdinaryText bool   `json:"ordinary_text"`
	OtherOutput  bool   `json:"other_output"`
	Fallback     bool   `json:"fallback"`
	FinishReason string `json:"finish_reason,omitempty"`
	Complete     bool   `json:"complete"`
	End          string `json:"end"`
}

func (value AntiTruncationObservation) Clone() AntiTruncationObservation {
	if value.Rounds != nil {
		value.Rounds = append([]AntiRoundObservation{}, value.Rounds...)
	}
	return value
}

type antiObservationKey struct{}
type antiObserverKey struct{}
type generationPurposeKey struct{}

// ContextWithAntiTruncationObserver installs a concurrent-safe snapshot consumer.
func ContextWithAntiTruncationObserver(ctx context.Context, observer func(AntiTruncationObservation)) context.Context {
	return context.WithValue(ctx, antiObserverKey{}, observer)
}

// ContextWithGenerationPurpose marks only the existing auxiliary summary path.
func ContextWithGenerationPurpose(ctx context.Context, purpose string) context.Context {
	if purpose != "compaction" {
		purpose = "reply"
	}
	return context.WithValue(ctx, generationPurposeKey{}, purpose)
}

var antiObservationSequence atomic.Uint64

type antiObservation struct {
	mu       sync.Mutex
	value    AntiTruncationObservation
	callback func(AntiTruncationObservation)
	name     string // exact injected name is private, never part of the snapshot
	active   string
	finished bool
}

func beginAntiObservation(ctx context.Context, request GenerateRequest, prompt bool) (context.Context, *antiObservation) {
	if !request.Selection.AntiTruncation {
		return ctx, nil
	}
	purpose, _ := ctx.Value(generationPurposeKey{}).(string)
	if purpose == "" {
		purpose = "reply"
	}
	observer, _ := ctx.Value(antiObserverKey{}).(func(AntiTruncationObservation))
	o := &antiObservation{callback: observer, value: AntiTruncationObservation{
		Version: AntiObservationVersion, ID: fmt.Sprintf("%s/transport-%d", request.ID, antiObservationSequence.Add(1)),
		RequestID: request.ID, Purpose: purpose, Behavior: antiBehaviorVersion, SystemPrompt: prompt,
		RequestedModel: request.Selection.RequestedModel, BaseModel: request.Selection.BaseModel,
		Channel: string(request.Selection.Channel), End: "preparing", Rounds: []AntiRoundObservation{},
	}}
	o.change(func(*AntiTruncationObservation) {})
	return context.WithValue(ctx, antiObservationKey{}, o), o
}

func (o *antiObservation) change(fn func(*AntiTruncationObservation)) {
	if o == nil {
		return
	}
	o.mu.Lock()
	fn(&o.value)
	o.value.Revision++
	snapshot := o.value.Clone()
	o.mu.Unlock()
	if o.callback != nil {
		o.callback(snapshot)
	}
}

func (o *antiObservation) injected(name string) {
	o.change(func(v *AntiTruncationObservation) { o.name = name; v.Injected = true })
}

func (o *antiObservation) round(id string, fn func(*AntiRoundObservation)) {
	o.change(func(v *AntiTruncationObservation) {
		for i := range v.Rounds {
			if v.Rounds[i].AttemptID == id {
				fn(&v.Rounds[i])
				return
			}
		}
	})
}

func (o *antiObservation) attempt(value GenerationAttempt) {
	o.change(func(v *AntiTruncationObservation) {
		i := 0
		for i < len(v.Rounds) && v.Rounds[i].AttemptID != value.ID {
			i++
		}
		if i == len(v.Rounds) {
			v.Rounds = append(v.Rounds, AntiRoundObservation{AttemptID: value.ID, Round: value.ContinuationRound, End: "preparing"})
		}
		r := &v.Rounds[i]
		r.Sent = r.Sent || value.Sent
		r.Response = r.Response || value.ResponseReceived
		switch value.Stage {
		case "http_error":
			r.End = "http_error"
		case "preparation_failed":
			r.End = "not_sent"
		case "failed":
			r.End = "incomplete"
		case "cancelled":
			r.End = "cancelled"
		case "timeout":
			r.End = "timeout"
		}
		if value.StatusCode != 0 {
			r.Status = value.StatusCode
		}
		v.Sent = v.Sent || value.Sent
		if value.Channel != "" {
			v.Channel = string(value.Channel)
		}
	})
}

func (o *antiObservation) raw(id string, event Event) {
	if o == nil {
		return
	}
	switch event.Kind {
	case EventToolCall, EventText, EventFinish, EventExecutableCode, EventCodeExecutionResult, EventMedia, EventGrounding, EventCitation:
	default:
		return
	}
	if event.Kind == EventText {
		if event.Text == "" {
			return
		}
		o.mu.Lock()
		seen := false
		for _, r := range o.value.Rounds {
			if r.AttemptID == id {
				seen = r.OrdinaryText
				break
			}
		}
		o.mu.Unlock()
		if seen {
			return
		} // record presence once, not a snapshot for every text token
	}
	o.change(func(v *AntiTruncationObservation) {
		o.active = id
		for i := range v.Rounds {
			r := &v.Rounds[i]
			if r.AttemptID != id {
				continue
			}
			switch event.Kind {
			case EventToolCall:
				if event.ToolCall != nil {
					if event.ToolCall.Name == o.name && o.name != "" {
						r.Calls++
					} else {
						r.RealTools++
					}
				}
			case EventText:
				r.OrdinaryText = r.OrdinaryText || event.Text != ""
			case EventExecutableCode, EventCodeExecutionResult, EventMedia, EventGrounding, EventCitation:
				r.OtherOutput = true
			case EventFinish:
				r.FinishReason = antiObservedFinishReason(event.FinishReason)
			}
		}
	})
}

// Preserve normalized enum labels, never arbitrary finish text or messages.
func antiObservedFinishReason(value string) string {
	if len(value) == 0 {
		return ""
	}
	if len(value) > 80 {
		return "provider_unknown"
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "provider_unknown"
		}
	}
	if suffix, ok := strings.CutPrefix(value, "provider_"); ok {
		if suffix == "" {
			return "provider_unknown"
		}
		for _, r := range suffix {
			if r < '0' || r > '9' {
				return "provider_unknown"
			}
		}
		return value
	}
	for code := int64(0); code < 19; code++ {
		if decodeFinishReason(code) == value {
			return value
		}
	}
	return "provider_unknown"
}

func (o *antiObservation) projected(result string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	id := o.active
	o.mu.Unlock()
	o.round(id, func(r *AntiRoundObservation) {
		switch result {
		case "legal":
			r.Legal++
		case "empty":
			r.Legal++
			r.Empty++
		case "invalid":
			r.Invalid++
		case "fallback":
			r.Fallback = true
		default:
			r.Rejected++
			r.RejectReason = result
		}
	})
}

func antiEnd(err error) string {
	switch {
	case err == nil:
		return "finished"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errStopSequenceMatched):
		return "local_stop"
	case errors.Is(err, errAntiTruncationMissingTerminal):
		return "missing_terminal"
	default:
		return "incomplete"
	}
}

func (o *antiObservation) decoded(id string, err error, continued bool) {
	o.round(id, func(r *AntiRoundObservation) {
		r.Complete = err == nil && (continued || r.FinishReason != "")
		r.End = antiEnd(err)
		if err == nil && continued {
			r.End = "continued"
		}
	})
}

func (o *antiObservation) finish(err error) {
	o.change(func(v *AntiTruncationObservation) {
		if o.finished {
			return
		}
		o.finished = true
		v.End = antiEnd(err)
		v.Complete = err == nil && v.Sent && len(v.Rounds) > 0
		for _, r := range v.Rounds {
			// An unsupported unary send followed by a supported fallback need not
			// erase positive evidence, but cannot be called complete coverage.
			v.Complete = v.Complete && r.Complete
		}
		if !v.Sent {
			v.End = "not_sent"
		}
	})
}

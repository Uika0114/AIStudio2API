package aistudio

import "context"

type rawGenerationObserverKey struct{}

// Observers run before projection and before an emitter can stop at a local
// stop sequence. Usage already decoded from the same frame must not disappear.
func contextWithRawGenerationObserver(ctx context.Context, observe func(Event)) context.Context {
	return context.WithValue(ctx, rawGenerationObserverKey{}, observe)
}

func reportRawGenerationEvent(ctx context.Context, event Event) {
	if event.Kind != EventError {
		observeGenerationProgress(ctx)
	}
	if observer, ok := ctx.Value(rawGenerationObserverKey{}).(func(Event)); ok && observer != nil {
		observer(event)
	}
}

type rawGenerationState struct {
	usage         *Usage
	finish        *Event
	output        generatedOutputParts
	observeOutput bool
}

func (state *rawGenerationState) observe(event Event) {
	switch event.Kind {
	case EventUsage:
		if event.Usage != nil {
			value := *event.Usage
			value.Source = "upstream"
			state.usage = &value
		}
	case EventFinish:
		value := event
		state.finish = &value
	default:
		if state.observeOutput {
			state.output.observe(event)
		}
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func newResponseSchemaTrackedService(t *testing.T) *trackedService {
	t.Helper()
	pool := aistudio.NewAccountPool(nil, 1)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	pooled, err := aistudio.NewPooledService(pool, &aistudio.Client{})
	if err != nil {
		t.Fatal(err)
	}
	service := &trackedService{
		lifecycle: t.Context(), dataContext: t.Context(), timeout: time.Second,
		pool: pool, service: pooled, requests: newRequestRegistry(t.Context()),
	}
	service.state.Store(serviceRunning) // In-memory lifecycle only; never Start a runtime.
	return service
}

func TestTrackedResponseSchemaErrorBeforeLease(t *testing.T) {
	for _, model := range []string{"fixture-model", "playground/fixture-model", "build/fixture-model"} {
		t.Run(model, func(t *testing.T) {
			service := newResponseSchemaTrackedService(t)
			var leases atomic.Int64
			service.leaseAcquire = func(context.Context, aistudio.AccountSelection) (*aistudio.AccountLease, error) {
				leases.Add(1)
				return nil, errors.New("unexpected lease acquisition")
			}
			for _, schema := range []string{
				`{"type":"array","items":{"type":"string"},"min_items":1,"minItems":2}`,
				`{"type":"array","items":{"type":"string"},"minItems":-1}`,
			} {
				request := aistudio.GenerateRequest{
					ID: t.Name() + schema, Model: model,
					Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "fixture"}}}},
					Config:   aistudio.GenerationConfig{ResponseSchema: json.RawMessage(schema)},
				}
				if _, err := service.Generate(t.Context(), request); !errors.Is(err, aistudio.ErrInvalidArgument) {
					t.Fatalf("tracked service did not synchronously return a parameter error: %v", err)
				}
			}
			if leases.Load() != 0 {
				t.Fatalf("schema errors acquired %d account leases", leases.Load())
			}
		})
	}
}

func TestTrackedResponseSchemaRouting(t *testing.T) {
	const canonical = `{"anyOf":[{"type":"OBJECT"},{"type":"OBJECT"}]}`
	const sdk = `{"any_of":[{"type":"OBJECT"},{"type":"OBJECT"}]}`
	for _, test := range []struct {
		name, model, schema string
		channel             aistudio.Channel
		invalid             bool
	}{
		{"canonical auto", "fixture-model", canonical, "", false},
		{"sdk auto", "fixture-model", sdk, "", false},
		{"open auto", "fixture-model", `true`, aistudio.ChannelBuild, false},
		{"strict PG", "playground/fixture-model", sdk, aistudio.ChannelPlayground, false},
		{"strict Build", "build/fixture-model", `true`, aistudio.ChannelBuild, false},
		{"strict open PG", "playground/fixture-model", `true`, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newResponseSchemaTrackedService(t)
			halt := errors.New("stop at the isolated lease boundary")
			acquisitions := 0
			var selected aistudio.AccountSelection
			service.leaseAcquire = func(_ context.Context, selection aistudio.AccountSelection) (*aistudio.AccountLease, error) {
				acquisitions++
				selected = selection
				return nil, halt
			}
			request, err := aistudio.NormalizeGenerateRequestModel(aistudio.GenerateRequest{
				ID: t.Name(), Model: test.model, Config: aistudio.GenerationConfig{ResponseSchema: json.RawMessage(test.schema)},
			})
			if err != nil {
				t.Fatal(err)
			}
			service.requests.start(request, func() {})
			events := make(chan aistudio.Event, 1)
			// Only lease acquisition is isolated; actual app selection and core schema judgment run.
			service.generateWithRetry(t.Context(), t.Context(), func() {}, time.Now(), request, "", events)
			failure := <-events
			if test.invalid {
				if acquisitions != 0 || !errors.Is(failure.Err, aistudio.ErrInvalidArgument) {
					t.Fatalf("strict PG guard bypassed: acquisitions=%d error=%v", acquisitions, failure.Err)
				}
			} else if acquisitions != 1 || selected.Channel != test.channel || selected.StrictChannel != request.Selection.Strict() || !errors.Is(failure.Err, halt) {
				t.Fatalf("app routing changed: acquisitions=%d selection=%+v error=%v", acquisitions, selected, failure.Err)
			}
		})
	}
}

package aistudio

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func fixtureOutputTiming() OutputTiming {
	return OutputTiming{
		Version: OutputTimingVersion, Method: OutputTimingMethod,
		AttemptID: "fixture/attempt-1", RequestID: "fixture", LifecycleID: "fixture/lifetime", Purpose: "reply",
		Status: OutputTimingMeasured, Complete: true, Terminal: true, HasOutput: true,
		ProgressBatches: 3, StartOutputTokens: 100, EndOutputTokens: 620,
		TokenSource: "upstream", DurationMS: 8000,
	}
}

func TestOutputTimingWindowProjection(t *testing.T) {
	timing := fixtureOutputTiming()
	got := timing.TPS()
	if got == nil || *got != 65 {
		t.Fatalf("window TPS=%v, want (620-100)/8=65, not 620/8", got)
	}
	// Fast values are not clipped by a presumed provider identity or route.
	timing.EndOutputTokens = 1100
	if got = timing.TPS(); got == nil || *got != 125 {
		t.Fatalf("fast window TPS=%v, want 125", got)
	}
	timing.EndOutputTokens, timing.DurationMS = 101, 100000
	if got = timing.TPS(); got == nil || *got != 0.01 {
		t.Fatalf("small positive window TPS=%v, want .01", got)
	}
	var missing *OutputTiming
	if missing.TPS() != nil {
		t.Fatal("absent timing must remain unknown, not zero")
	}
}

func TestOutputTimingRejectsUnprovenOrMalformedWindow(t *testing.T) {
	cases := map[string]func(*OutputTiming){
		"unknown version":                func(v *OutputTiming) { v.Version++ },
		"unproven method":                func(v *OutputTiming) { v.Method = "server-timing/gfet4t7" },
		"unavailable":                    func(v *OutputTiming) { v.Status = OutputTimingUnavailable },
		"incomplete status":              func(v *OutputTiming) { v.Status = OutputTimingIncomplete },
		"unattributed":                   func(v *OutputTiming) { v.Status = OutputTimingUnattributed },
		"unknown status":                 func(v *OutputTiming) { v.Status = "other" },
		"reason conflicts with measured": func(v *OutputTiming) { v.Reason = "counter_missing" },
		"unfinished coverage":            func(v *OutputTiming) { v.Complete = false },
		"no nonthinking output":          func(v *OutputTiming) { v.HasOutput = false },
		"no attempt identity":            func(v *OutputTiming) { v.AttemptID = "" },
		"unknown purpose":                func(v *OutputTiming) { v.Purpose = "" },
		"local estimate":                 func(v *OutputTiming) { v.TokenSource = "local" },
		"unattributed tokens":            func(v *OutputTiming) { v.TokenSource = "unknown" },
		"one batch":                      func(v *OutputTiming) { v.ProgressBatches = 1 },
		"negative baseline":              func(v *OutputTiming) { v.StartOutputTokens = -1 },
		"zero increment":                 func(v *OutputTiming) { v.EndOutputTokens = v.StartOutputTokens },
		"regressed counter":              func(v *OutputTiming) { v.EndOutputTokens = 20 },
		"zero duration":                  func(v *OutputTiming) { v.DurationMS = 0 },
		"negative duration":              func(v *OutputTiming) { v.DurationMS = -1 },
		"nan duration":                   func(v *OutputTiming) { v.DurationMS = math.NaN() },
		"infinite duration":              func(v *OutputTiming) { v.DurationMS = math.Inf(1) },
		"overflowing quotient":           func(v *OutputTiming) { v.DurationMS = math.SmallestNonzeroFloat64 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			timing := fixtureOutputTiming()
			mutate(&timing)
			if got := timing.TPS(); got != nil {
				t.Fatalf("unqualified evidence produced TPS=%v", *got)
			}
		})
	}
}

func TestOutputTimingBodyFreeSnapshot(t *testing.T) {
	timing := fixtureOutputTiming()
	data, err := json.Marshal(timing)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"signature", "arguments", "reasoning_tokens", "text", "http_duration", "first_event"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("nonthinking window schema unexpectedly contains %q", forbidden)
		}
	}
}

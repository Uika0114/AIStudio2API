package aistudio

import (
	"encoding/json"
	"strings"
	"testing"
)

// Modify only synthetic raw usage, so both protocols exercise presence before
// public usage defaults can turn missing thoughts into zero or cached values.
func outputReasoningFrame(t *testing.T, channel, text string, count int64, reasoning *int64, finish bool) string {
	t.Helper()
	if channel == "playground" {
		var frame []any
		if err := json.Unmarshal([]byte(outputPGFrame(text, outputCount(count), finish, false)), &frame); err != nil {
			t.Fatal(err)
		}
		usage := frame[2].([]any)
		usage[9] = reasoning
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	var wrapper []string
	if err := json.Unmarshal([]byte(outputBuildFrame(text, outputCount(count), finish, false)), &wrapper); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(wrapper[0]), &response); err != nil {
		t.Fatal(err)
	}
	if reasoning != nil {
		response["usageMetadata"].(map[string]any)["thoughtsTokenCount"] = *reasoning
	}
	inner, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]string{string(inner)})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOutputWindowFreshReasoningCounterWithoutSummary(t *testing.T) {
	type step struct {
		text      string
		output    int64
		reasoning *int64
		at        int
	}
	cases := []struct {
		name  string
		steps []step
		want  *float64
	}{
		{"growth_interrupts", []step{{"a", 100, outputCount(100), 0}, {"", 100, outputCount(200), 4}, {"b", 620, outputCount(200), 8}}, nil},
		{"growth_then_new_window", []step{{"a", 100, outputCount(100), 0}, {"", 100, outputCount(200), 4}, {"b", 620, outputCount(200), 8}, {"c", 760, outputCount(200), 10}}, func() *float64 { v := 70.0; return &v }()},
		{"missing_reasoning_is_not_a_gate", []step{{"a", 100, nil, 0}, {"", 100, nil, 4}, {"b", 620, nil, 8}}, func() *float64 { v := 65.0; return &v }()},
		{"first_nonzero_is_not_growth_from_unknown", []step{{"a", 100, nil, 0}, {"b", 620, outputCount(200), 8}}, func() *float64 { v := 65.0; return &v }()},
		{"missing_middle_is_not_zero_or_cached_progress", []step{{"a", 100, outputCount(100), 0}, {"", 100, nil, 4}, {"b", 620, outputCount(100), 8}}, func() *float64 { v := 65.0; return &v }()},
		{"explicit_zero_is_known", []step{{"a", 100, outputCount(0), 0}, {"", 100, outputCount(200), 4}, {"b", 620, outputCount(200), 8}}, nil},
		{"reappearing_same_counter_is_not_new_progress", []step{{"a", 100, outputCount(100), 0}, {"", 100, outputCount(200), 4}, {"b", 620, nil, 8}, {"c", 760, outputCount(200), 10}}, func() *float64 { v := 70.0; return &v }()},
	}
	for _, channel := range []string{"playground", "build"} {
		for _, test := range cases {
			t.Run(channel+"/"+test.name, func(t *testing.T) {
				var chunks []outputFixtureChunk
				for i, step := range test.steps {
					frame := outputReasoningFrame(t, channel, step.text, step.output, step.reasoning, i == len(test.steps)-1)
					if i == 0 {
						frame = "[[" + frame
					}
					if i == len(test.steps)-1 {
						frame += "]]"
					} else {
						frame += ","
					}
					chunks = append(chunks, outputFixtureChunk{frame, step.at})
				}
				var got *OutputTiming
				var err error
				if channel == "playground" {
					var events []Event
					got, events, err, _ = decodeOutputFixture(t, chunks, true)
					for _, event := range events {
						if event.Kind == EventReasoning && strings.TrimSpace(event.Text) != "" {
							t.Fatal("fixture accidentally returned a reasoning summary")
						}
					}
				} else {
					got, err = decodeBuildOutputFixture(t, chunks)
				}
				if err != nil {
					t.Fatal(err)
				}
				value := got.TPS()
				if test.want == nil {
					if value != nil {
						t.Fatalf("must not cross proven hidden-thinking growth: %+v", got)
					}
				} else if value == nil || *value != *test.want {
					t.Fatalf("wanted %v without requiring reasoning presence: %+v", *test.want, got)
				}
			})
		}
	}
}

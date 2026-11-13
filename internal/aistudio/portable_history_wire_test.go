package aistudio

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPortableToolHistoryWireSignature(t *testing.T) {
	for _, signature := range []string{"", "valid-same-route"} {
		t.Run(signature, func(t *testing.T) {
			part := Part{FunctionCall: &FunctionCall{ID: "call-real", Name: "real", Arguments: json.RawMessage(`{"value":1}`), ThoughtSignature: signature}}
			playground, err := encodePart(part)
			if err != nil {
				t.Fatal(err)
			}
			build, err := encodeBuildPart(part)
			if err != nil {
				t.Fatal(err)
			}
			want := signature
			if want == "" {
				want = "skip_thought_signature_validator"
			}
			if len(playground) <= 14 || playground[14] != want || build["thoughtSignature"] != want {
				t.Fatal("wrong tool signature encoding")
			}

			signed := part
			call := *part.FunctionCall
			signed.FunctionCall = &call
			signed.FunctionCall.ThoughtSignature = "comparison"
			signedPlayground, err := encodePart(signed)
			if err != nil {
				t.Fatal(err)
			}
			signedBuild, err := encodeBuildPart(signed)
			if err != nil {
				t.Fatal(err)
			}
			signedPlayground[14], signedBuild["thoughtSignature"] = want, want
			if !reflect.DeepEqual(playground, signedPlayground) || !reflect.DeepEqual(build, signedBuild) {
				t.Fatal("wire tool semantics changed beyond the signature")
			}
		})
	}
}

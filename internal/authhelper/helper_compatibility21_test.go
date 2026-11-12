package authhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Frozen consumer schemas copied from 96cba3a3a46b9e47da58ac276a04ed0b2217e004:
// internal/authhelper/protocol.go and internal/aistudio/auth.go. Do not alias
// current wire types: this test must reject any future added diagnostic fields.
type consumerV1Request21 struct {
	Version      int    `json:"version"`
	Handle       string `json:"handle"`
	Epoch        uint64 `json:"mapping_epoch"`
	OperationID  string `json:"operation_id"`
	BaseCookie   uint64 `json:"base_cookie_revision"`
	BaseMaterial uint64 `json:"base_material_version"`
	Reason       string `json:"reason"`
	Deadline     int64  `json:"deadline"`
	LookupOnly   bool   `json:"lookup_only,omitempty"`
}

type consumerV1Cookie21 struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"`
	HTTPOnly     bool    `json:"httpOnly"`
	Secure       bool    `json:"secure"`
	SameSite     string  `json:"sameSite"`
	PartitionKey string  `json:"partitionKey,omitempty"`
}

type consumerV1Response21 struct {
	Version         int                  `json:"version"`
	Handle          string               `json:"handle"`
	Epoch           uint64               `json:"mapping_epoch"`
	OperationID     string               `json:"operation_id"`
	BaseCookie      uint64               `json:"base_cookie_revision"`
	BaseMaterial    uint64               `json:"base_material_version"`
	CookieRevision  uint64               `json:"cookie_revision"`
	MaterialVersion uint64               `json:"material_version"`
	GaiaID          string               `json:"gaia_id"`
	Email           string               `json:"email"`
	Cookies         []consumerV1Cookie21 `json:"cookies"`
}

type consumerV1Result21 struct {
	Code       string                `json:"code"`
	Session    *consumerV1Response21 `json:"session,omitempty"`
	NotStarted *consumerV1Request21  `json:"not_started,omitempty"`
}

func decodeConsumerV1Result21(body io.Reader) (consumerV1Result21, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var result consumerV1Result21
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("fixture trailing response")
	}
	return result, nil
}

func TestNewHelperRepliesFitFrozenConsumerV1StrictSchema(t *testing.T) {
	for _, mode := range []string{"ready", "not-started", "indeterminate", "identity", "cookie-policy"} {
		t.Run(mode, func(t *testing.T) {
			b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
				switch mode {
				case "indeterminate":
					return s, errors.New("fixture-private-transport-error")
				case "identity":
					return s, failure("IDENTITY_MISMATCH")
				case "cookie-policy":
					return s, failure("COOKIE_POLICY_FAILURE")
				default:
					return s, nil
				}
			})
			c, server := fixtureHTTP(t, b, m)
			r := fixtureRequest(m, "frozen_consumer_v1_0001", 0)
			oldRequest := consumerV1Request21{Version: 1, Handle: r.Handle, Epoch: r.Epoch, OperationID: r.OperationID, BaseCookie: r.BaseCookie, BaseMaterial: r.BaseMaterial, Reason: r.Reason, Deadline: r.Deadline, LookupOnly: mode == "not-started"}
			body, err := json.Marshal(oldRequest)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/refresh", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			response, err := c.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			oldResult, err := decodeConsumerV1Result21(response.Body)
			if err != nil {
				t.Fatal("new helper broke frozen strict consumer schema", err)
			}
			want := map[string]string{"ready": "OK", "not-started": "NOT_STARTED", "indeterminate": "INDETERMINATE", "identity": "IDENTITY_MISMATCH", "cookie-policy": "COOKIE_POLICY_FAILURE"}[mode]
			if oldResult.Code != want {
				t.Fatal("safe v1 failure contract changed")
			}
			if mode == "ready" && (oldResult.Session == nil || oldResult.Session.CookieRevision != 1 || len(oldResult.Session.Cookies) != 3) {
				t.Fatal("old consumer cannot receive ready projection")
			}
			if mode == "not-started" {
				if oldResult.NotStarted == nil || *oldResult.NotStarted != oldRequest || count.Load() != 0 {
					t.Fatal("old consumer lost exact durable no-auth receipt")
				}
			} else if oldResult.NotStarted != nil || count.Load() != 1 {
				t.Fatal("failure fabricated no-auth receipt or repeated renewal")
			}
			// The strict durable session parser is byte-for-byte unchanged from
			// the baseline and must accept each new operation's existing schema.
			state, err := aistudio.LoadStorageState(m.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := state.Session(); err != nil {
				t.Fatal("new helper persisted fields rejected by old session parser", err)
			}
		})
	}
	for _, data := range []string{
		`{"code":"INDETERMINATE","diagnostic":"not-in-v1"}`,
		`{"code":"OK","session":{"phase":"not-in-v1"}}`,
	} {
		if _, err := decodeConsumerV1Result21(bytes.NewBufferString(data)); err == nil {
			t.Fatal("compatibility fixture stopped enforcing strict fields")
		}
	}
}

package app

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestRemoteQueryRuntimePreparationFailureDoesNotCommitOrAck(t *testing.T) {
	for _, stage := range []string{"worker-reset", "header-prepare"} {
		t.Run(stage, func(t *testing.T) {
			f := newQueryAppFixture(t, "new", "reauth", false)
			before, e := aistudio.LoadStorageState(f.path)
			if e != nil {
				t.Fatal(e)
			}
			if stage == "worker-reset" {
				f.refresher.reset = func(string) error { f.resets++; return errors.New("invented reset failure") }
			} else {
				f.refresher.prepareHeaders = func(string) (func(bool), error) { f.headers++; return nil, errors.New("invented header failure") }
			}
			e = f.refresher.Refresh(f.ctx)
			if !aistudio.IsSessionFailureCode(e, "STATE_UNAVAILABLE") {
				t.Fatal("runtime failure lost safe boundary", e)
			}
			after, e := aistudio.LoadStorageState(f.path)
			if e != nil {
				t.Fatal(e)
			}
			meta, _ := after.Session()
			if !reflect.DeepEqual(before.Cookies, after.Cookies) || meta.Remote.Pending == nil || meta.Remote.Ack != nil || meta.Remote.CookieRevision != 0 || f.ack.Load() != 0 || f.auth.Load() != 1 {
				t.Fatal("failed runtime preparation committed or ACKed projection")
			}
		})
	}
}

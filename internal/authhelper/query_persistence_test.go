package authhelper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestQueryFailedSessionLockRetirementPreservesCallerAndDisk(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p, _ := queryIntent(t, m, "bootstrap", true)
	before, _ := os.ReadFile(p)
	// The original file fits the component limit, but its .session.lock
	// suffix does not. This specifically tests failure before atomic replacement.
	p = filepath.Join(t.TempDir(), strings.Repeat("q", 243)+".json")
	if err := os.WriteFile(p, before, 0600); err != nil {
		t.Fatal(err)
	}
	memory, _ := json.Marshal(s)
	err := c.PrepareRecovery(t.Context(), &s, p, "bootstrap", nil)
	after, _ := os.ReadFile(p)
	currentMemory, _ := json.Marshal(s)
	if err == nil || string(memory) != string(currentMemory) || string(before) != string(after) || count.Load() != 0 {
		t.Fatal("failed retirement left caller/disk falsely resolved", err)
	}
}

func TestQueryHelperAcceptedPendingCrashRemainsUnknownAfterDeadline(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p, req := queryIntent(t, m, "reauth", true)
	authority, _ := aistudio.LoadStorageState(m.StateFile)
	meta, _ := authority.Session()
	meta.Authority.Operation = &aistudio.SessionOperation{ID: req.OperationID, Reason: req.Reason, Epoch: req.Epoch, BaseCookie: req.BaseCookie, BaseMaterial: req.BaseMaterial, Deadline: req.Deadline, Stage: "pending"}
	if authority.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &authority) != nil {
		t.Fatal("accepted intent fixture")
	}
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	before, _ := os.ReadFile(p)
	if err := c.PrepareRecovery(t.Context(), &s, p, "reauth", nil); safeCode(err) != "INDETERMINATE" {
		t.Fatal("accepted pending changed into no-send", err)
	}
	after, _ := os.ReadFile(p)
	if count.Load() != 0 || string(before) != string(after) {
		t.Fatal("crash recovery sent authentication or changed intent")
	}
}

func TestQueryAuthorityDurablyKeepsResultAfterConsumerCancellation(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p := fixtureRemoteState(t, m)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	persisted := make(chan error, 1)
	write := b.write
	b.write = func(ctx context.Context, path string, value *aistudio.StorageState) error {
		e := write(ctx, path, value)
		meta, _ := value.Session()
		if meta != nil && meta.Authority != nil && meta.Authority.Operation != nil && meta.Authority.Operation.Stage == "ready" {
			persisted <- e
		}
		return e
	}
	b.renew = func(_ context.Context, in aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
		count.Add(1)
		cancel()
		in.Cookies = fixtureCookies("late-result-private")
		return in, nil
	}
	err := c.PrepareRecovery(ctx, &s, p, "reauth", func(string) error { return nil })
	if safeCode(err) != "CANCELED" {
		t.Fatal(err)
	}
	// Wait for the authority write itself, not a racy observer polling a file
	// that Windows may temporarily deny opening while atomic rename commits.
	select {
	case e := <-persisted:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authority did not persist completed result")
	}
	disk, _ := aistudio.LoadStorageState(p)
	dm, _ := disk.Session()
	if dm.Remote.Pending == nil || dm.Remote.Ack != nil || len(disk.Cookies) != 0 {
		t.Fatal("canceled consumer committed late result")
	}
	if err := c.PrepareRecovery(t.Context(), &disk, p, "reauth", nil); err != nil {
		t.Fatal("late result not retrievable by original ID", err)
	}
	if count.Load() != 1 {
		t.Fatal("query repeated authentication")
	}
}

func TestQueryBudgetExhaustionDoesNotPersistNewIntent(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p := fixtureRemoteState(t, m)
	before, _ := os.ReadFile(p)
	if err := c.PrepareRecovery(t.Context(), &s, p, "reauth", func(string) error { return failure("RECOVERY_BUDGET_EXHAUSTED") }); safeCode(err) != "RECOVERY_BUDGET_EXHAUSTED" {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) || count.Load() != 0 {
		t.Fatal("exhausted budget created durable auth work")
	}
}

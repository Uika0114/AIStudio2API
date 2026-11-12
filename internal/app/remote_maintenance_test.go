package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func failRemoteIntent(t *testing.T, r *authRuntimeRefresher, fixture *remoteProviderFixture, calls *int) {
	t.Helper()
	fixture.prepare = func(ctx context.Context, state *aistudio.StorageState, path, reason string, take func(string) error) error {
		meta, _ := state.Session()
		if meta.Remote.Pending == nil {
			if err := take("quarantine_operation_0001"); err != nil {
				return err
			}
			*calls++
			meta.Remote.Pending = &aistudio.SessionOperation{ID: "quarantine_operation_0001", Reason: reason, Epoch: meta.Remote.Epoch, BaseCookie: meta.Remote.CookieRevision, BaseMaterial: meta.Remote.MaterialVersion, StateRevision: meta.Revision + 1, Deadline: r.recoveryNow().Add(time.Minute).Unix()}
			meta.Remote.RecoveryRequired = true
			if err := state.SetSession(meta); err != nil {
				return err
			}
			if err := aistudio.WriteManagedSessionState(ctx, path, state); err != nil {
				return err
			}
		}
		return aistudio.SessionError("INDETERMINATE")
	}
}

func TestRemoteMaintenanceFailureQuarantinesBeforeRelease(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	now := time.Now().UTC()
	r.recoveryClock = func() time.Time { return now }
	failRemoteIntent(t, r, fixture, calls)
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	if err := r.Refresh(ctx); !aistudio.IsSessionFailureCode(err, "INDETERMINATE") {
		t.Fatal(err)
	}
	if status := r.pool.Status()[0]; status.State != aistudio.AccountAuthRequired || !status.Enabled {
		t.Fatal("failure not published before release", status)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if next, acquired, err := r.pool.TryAcquireFor(t.Context(), aistudio.AccountSelection{AccountID: lease.Account().ID}); acquired || next != nil {
		t.Fatal("second independent request acquired quarantined account", acquired, err)
	}
	// A restart reconstructs isolation from session, not cached models or control.
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	old := lease.Account()
	restarted := &aistudio.Account{ID: old.ID, Directory: old.Directory, StoragePath: old.StoragePath, RuntimePath: old.RuntimePath, Config: old.Config, StorageState: state}
	pool := aistudio.NewAccountPool([]*aistudio.Account{restarted}, 1)
	if pool.Status()[0].State != aistudio.AccountAuthRequired {
		t.Fatal("restart forgot quarantine")
	}
	if *calls != 1 {
		t.Fatal("unexpected authentication workflows", *calls)
	}
}

func TestRemoteMaintenancePendingBoundedAndDisabled(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	now := time.Now().UTC()
	r.recoveryClock = func() time.Time { return now }
	failRemoteIntent(t, r, fixture, calls)
	if err := r.Refresh(ctx); !aistudio.IsSessionFailureCode(err, "INDETERMINATE") {
		t.Fatal(err)
	}
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	lease.Account().ConfigPath = filepath.Join(lease.Account().Directory, "fixture-account.json")
	config := lease.Account().Config
	config.Enabled = false
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	queries := 0
	fixture.prepare = func(_ context.Context, s *aistudio.StorageState, _, _ string, _ func(string) error) error {
		meta, _ := s.Session()
		if meta.Remote.Pending == nil || meta.Remote.Pending.ID != "quarantine_operation_0001" {
			t.Fatal("continuation changed operation")
		}
		queries++
		return aistudio.SessionError("INDETERMINATE")
	}
	r.runRemoteMaintenance(t.Context())
	if queries != 0 {
		t.Fatal("ignored initial backoff")
	}
	now = now.Add(30 * time.Second)
	r.runRemoteMaintenance(t.Context())
	if queries != 0 || remoteRecoveryStatus(path, false, now).PendingUsed != 0 {
		t.Fatal("disabled account consumed automatic budget")
	}
	management, err := r.pool.AcquireAccount(t.Context(), lease.Account().ID)
	if err != nil {
		t.Fatal(err)
	}
	config.Enabled = true
	if err := management.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	management.Release()
	r.runRemoteMaintenance(t.Context())
	now = now.Add(2 * time.Minute)
	r.runRemoteMaintenance(t.Context())
	now = now.Add(5 * time.Minute)
	r.runRemoteMaintenance(t.Context())
	now = now.Add(time.Minute)
	r.runRemoteMaintenance(t.Context())
	status := remoteRecoveryStatus(path, true, now)
	if queries != 3 || *calls != 1 || status.Code != "RECOVERY_BUDGET_EXHAUSTED" || status.Queries != 3 {
		t.Fatal("pending continuation budget", queries, *calls, status)
	}
}

func TestRemoteMaintenanceReadyResultRecoversWithoutNewAuth(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	now := time.Now().UTC()
	r.recoveryClock = func() time.Time { return now }
	failRemoteIntent(t, r, fixture, calls)
	_ = r.Refresh(ctx)
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	lease.Release()
	queries, acks := 0, 0
	fixture.prepare = func(_ context.Context, s *aistudio.StorageState, _, _ string, _ func(string) error) error {
		queries++
		meta, _ := s.Session()
		id := meta.Remote.Pending.ID
		meta.Remote.Pending = nil
		meta.Remote.RecoveryRequired = false
		meta.Remote.CookieRevision++
		meta.Remote.Ack = &aistudio.SessionAck{ID: id, Epoch: meta.Remote.Epoch, Cookie: meta.Remote.CookieRevision, Material: meta.Remote.MaterialVersion}
		s.Cookies = fixtureRefreshCookies("ready-result-fixture")
		return s.SetSession(meta)
	}
	fixture.ack = func(context.Context, *aistudio.RemoteSession) error { acks++; return nil }
	now = now.Add(30 * time.Second)
	r.runRemoteMaintenance(t.Context())
	if queries != 1 || acks != 1 || *calls != 1 || r.pool.Status()[0].State != aistudio.AccountReady {
		t.Fatal("ready result not adopted", queries, acks, *calls, r.pool.Status())
	}
	state, _ := aistudio.LoadStorageState(path)
	meta, _ := state.Session()
	if meta.Remote.Pending != nil || meta.Remote.Ack != nil || meta.Remote.RecoveryRequired || state.Cookies[0].Value != "ready-result-fixture" {
		t.Fatal("incomplete recovery")
	}
}

func TestRemoteMaintenanceAckBudgetDoesNotQuarantine(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	now := time.Now().UTC()
	r.recoveryClock = func() time.Time { return now }
	acks := 0
	fixture.ack = func(context.Context, *aistudio.RemoteSession) error {
		acks++
		return aistudio.SessionError("HELPER_UNAVAILABLE")
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(ctx); !aistudio.IsSessionFailureCode(err, "RECOVERY_BACKOFF") || acks != 1 {
		t.Fatal("ordinary request bypassed ACK compensation budget", err, acks)
	}
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	lease.Release()
	for _, delay := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, time.Minute} {
		now = now.Add(delay)
		r.runRemoteMaintenance(t.Context())
		if r.pool.Status()[0].State != aistudio.AccountReady {
			t.Fatal("ACK failure quarantined valid projection")
		}
	}
	status := remoteRecoveryStatus(path, true, now)
	if acks != 4 || *calls != 1 || status.Code != "RECOVERY_BUDGET_EXHAUSTED" {
		t.Fatal("ACK was unbounded or became authentication", acks, *calls, status)
	}
	dto := adminAccountDTO(r.pool.Status()[0], r.pool)
	if dto.RemoteRecovery == nil || !dto.RemoteRecovery.AckPending || dto.RemoteRecovery.Code != "RECOVERY_BUDGET_EXHAUSTED" {
		t.Fatal("ACK/exhaustion diagnosis missing", dto)
	}
}

func TestRemoteMaintenanceHealthyLegacyHasNoBudgetWarning(t *testing.T) {
	r, _, _, _, _ := fixtureRemoteRefresher(t)
	dto := adminAccountDTO(r.pool.Status()[0], r.pool)
	if dto.RemoteRecovery != nil {
		t.Fatal("healthy legacy session shown as budget failure", dto)
	}
}

func TestRemoteMaintenanceAckConflictStopsReplay(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	fixture.ack = func(context.Context, *aistudio.RemoteSession) error {
		return aistudio.SessionError("VERSION_CONFLICT")
	}
	sends := 0
	_, err := r.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
		sends++
		return nil, &aistudio.RPCError{StatusCode: 401, Code: 16}
	})
	state, _ := aistudio.LoadStorageState(path)
	meta, _ := state.Session()
	if !aistudio.IsSessionFailureCode(err, "VERSION_CONFLICT") || sends != 1 || *calls != 1 ||
		!meta.Remote.RecoveryRequired || meta.Remote.Ack == nil || state.Cookies[0].Value != "fixture-remote-new" ||
		r.pool.Status()[0].State != aistudio.AccountAuthRequired {
		t.Fatal("ACK conflict replayed or discarded committed cookies", err, sends, *calls)
	}
}

func TestRemoteMaintenanceHistoricalPendingHasNoAutomaticBudget(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	err := lease.UpdateRemoteSession(ctx, func(meta *aistudio.SessionState) error {
		meta.Remote.Pending = &aistudio.SessionOperation{ID: "historical_operation_0001", Reason: "reauth", Epoch: meta.Remote.Epoch, BaseCookie: meta.Remote.CookieRevision, BaseMaterial: meta.Remote.MaterialVersion, StateRevision: meta.Revision + 1, Deadline: time.Now().Add(-time.Hour).Unix()}
		meta.Remote.RecoveryRequired = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.prepare = func(context.Context, *aistudio.StorageState, string, string, func(string) error) error {
		t.Fatal("historical pending reached helper without budget provenance")
		return nil
	}
	if err := r.Refresh(ctx); !aistudio.IsSessionFailureCode(err, "RECOVERY_BUDGET_REQUIRED") {
		t.Fatal(err)
	}
	lease.Release()
	r.runRemoteMaintenance(t.Context())
	if *calls != 0 || remoteRecoveryStatus(path, true, time.Now()).Action != "none" || r.pool.Status()[0].State != aistudio.AccountAuthRequired {
		t.Fatal("historical pending acquired automatic recovery allowance")
	}
}

func TestRemoteMaintenanceControlFinishFailureKeepsCommittedRecoveryReady(t *testing.T) {
	r, ctx, path, fixture, calls := fixtureRemoteRefresher(t)
	now := time.Now().UTC()
	r.recoveryClock = func() time.Time { return now }
	failRemoteIntent(t, r, fixture, calls)
	_ = r.Refresh(ctx)
	lease, _ := aistudio.AccountLeaseFromContext(ctx)
	lease.Release()
	fixture.prepare = func(_ context.Context, s *aistudio.StorageState, _, _ string, _ func(string) error) error {
		meta, _ := s.Session()
		id := meta.Remote.Pending.ID
		meta.Remote.Pending = nil
		meta.Remote.RecoveryRequired = false
		meta.Remote.CookieRevision++
		meta.Remote.Ack = &aistudio.SessionAck{ID: id, Epoch: meta.Remote.Epoch, Cookie: meta.Remote.CookieRevision, Material: meta.Remote.MaterialVersion}
		s.Cookies = fixtureRefreshCookies("committed-despite-control-fixture")
		return s.SetSession(meta)
	}
	r.prepareHeaders = func(string) (func(bool), error) {
		return func(committed bool) {
			if !committed {
				return
			}
			control := path + ".recovery-control.json"
			if err := os.Rename(control, control+".fixture-backup"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(control, 0700); err != nil {
				t.Fatal(err)
			}
		}, nil
	}
	now = now.Add(30 * time.Second)
	r.runRemoteMaintenance(t.Context())
	state, _ := aistudio.LoadStorageState(path)
	meta, _ := state.Session()
	if r.pool.Status()[0].State != aistudio.AccountReady || meta.Remote.Pending != nil || meta.Remote.RecoveryRequired ||
		state.Cookies[0].Value != "committed-despite-control-fixture" || *calls != 1 {
		t.Fatal("control finalization failure quarantined valid committed session")
	}
}

func TestRemoteMaintenanceTrustedNotStartedStartsLaterIndependentAuth(t *testing.T) {
	f := newQueryAppFixture(t, "not-started", "reauth", false)
	if err := f.refresher.Refresh(f.ctx); !aistudio.IsSessionFailureCode(err, "NOT_STARTED") {
		t.Fatal(err)
	}
	if f.auth.Load() != 0 || f.refresh.Load() != 1 || f.lookups.Load() != 1 {
		t.Fatal("original failed call started a replacement authentication")
	}
	lease, _ := aistudio.AccountLeaseFromContext(f.ctx)
	lease.Release()
	f.now = f.now.Add(time.Minute - time.Nanosecond)
	f.refresher.runRemoteMaintenance(t.Context())
	if f.auth.Load() != 0 {
		t.Fatal("NOT_STARTED backoff was bypassed")
	}
	f.now = f.now.Add(time.Nanosecond)
	f.refresher.runRemoteMaintenance(t.Context())
	state, _ := aistudio.LoadStorageState(f.path)
	meta, _ := state.Session()
	if f.auth.Load() != 1 || f.refresh.Load() != 2 || f.lookups.Load() != 1 || f.ack.Load() != 1 ||
		meta.Remote.Pending != nil || meta.Remote.RecoveryRequired || f.pool.Status()[0].State != aistudio.AccountReady {
		t.Fatal("independent maintenance did not consume the trusted retirement", f.auth.Load(), f.refresh.Load(), f.lookups.Load())
	}
}

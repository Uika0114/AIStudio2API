package aistudio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func remoteQuarantineFixture(t *testing.T, mutate func(*RemoteSession)) (*AccountPool, *Account) {
	t.Helper()
	state := sessionTestState(t, false)
	state.Cookies = nil
	for _, cookie := range signatureCookies {
		state.Cookies = append(state.Cookies, StateCookie{Name: cookie.name, Value: "synthetic-only", Domain: ".google.com", Path: "/", Expires: -1, Secure: true})
	}
	meta, _ := state.Session()
	meta.Remote.CookieRevision = 1
	if mutate != nil {
		mutate(meta.Remote)
	}
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "fixture@example.invalid")
	if err := WriteManagedSessionState(context.Background(), filepath.Join(directory, storageStateName), &state); err != nil {
		t.Fatal(err)
	}
	config := DefaultAccountConfig("fixture@example.invalid")
	if err := writeAccountConfig(filepath.Join(directory, accountConfigName), config); err != nil {
		t.Fatal(err)
	}
	account, err := loadAccount(directory)
	if err != nil {
		t.Fatal(err)
	}
	account.Models = []Model{{ID: "fixture-model", Methods: []string{"generateContent", "predictLongRunning"}, Capabilities: map[string]bool{"chat_model": true}}}
	return NewAccountPool([]*Account{account}, 2), account
}

func quarantinePending(remote *RemoteSession) {
	remote.Pending = &SessionOperation{
		ID: "fixture_pending_0001", Reason: "reauth", Epoch: remote.Epoch,
		BaseCookie: remote.CookieRevision, BaseMaterial: remote.MaterialVersion,
		Deadline: time.Now().Add(time.Minute).Unix(),
	}
}

func assertNoRemoteGeneration(t *testing.T, pool *AccountPool, account *Account) {
	t.Helper()
	for _, selection := range []AccountSelection{
		{AccountID: account.ID},
		{AccountID: account.ID, ModelID: "fixture-model", Method: "generateContent"},
		{AccountID: account.ID, ResourceID: "fixture-file"},
	} {
		lease, waitable, err := pool.TryAcquireFor(context.Background(), selection)
		if lease != nil {
			_ = lease.Release()
			t.Fatal("quarantined account received a new generation lease")
		}
		if waitable || err != nil && !errors.Is(err, ErrResourceNotFound) {
			t.Fatalf("quarantine should be unavailable, not busy: waitable=%v err=%v", waitable, err)
		}
	}
	for _, warm := range [][]string{nil, {account.ID}} {
		groups, err := pool.ClassifyCandidates(context.Background(), AccountSelection{ModelID: "fixture-model"}, warm)
		if err != nil || groups.Eligible || len(groups.WarmReady)+len(groups.StandbyReady)+len(groups.WarmBusy)+len(groups.StandbyBusy) != 0 {
			t.Fatalf("quarantine entered warm/standby candidates: %+v %v", groups, err)
		}
	}
}

func TestRemoteQuarantineRebuildAndAckException(t *testing.T) {
	for _, item := range []struct {
		name   string
		mutate func(*RemoteSession)
		code   string
	}{
		{"pending", quarantinePending, "PENDING_RECOVERY"},
		{"recovery", func(r *RemoteSession) { r.RecoveryRequired = true }, "RECOVERY_REQUIRED"},
		{"enrollment", func(r *RemoteSession) { r.EnrollmentRequired = true }, "BOOTSTRAP_REQUIRED"},
		{"ack", func(r *RemoteSession) {
			r.Ack = &SessionAck{ID: "fixture_ack_000001", Epoch: r.Epoch, Cookie: r.CookieRevision, Material: r.MaterialVersion}
		}, ""},
	} {
		t.Run(item.name, func(t *testing.T) {
			pool, account := remoteQuarantineFixture(t, item.mutate)
			if err := pool.BindResource("fixture-file", account.ID); err != nil {
				t.Fatal(err)
			}
			if item.code == "" {
				lease, err := pool.Acquire(context.Background(), "fixture-model")
				if err != nil {
					t.Fatal(err)
				}
				_ = lease.Release()
				if pool.Status()[0].State != AccountReady {
					t.Fatal("ACK alone quarantined valid projection")
				}
				return
			}
			assertNoRemoteGeneration(t, pool, account)
			if status := pool.Status()[0]; status.State != AccountAuthRequired || status.Message != item.code || !status.Enabled {
				t.Fatalf("bad quarantine projection: %+v", status)
			}
			// Management must still be able to disable even an unenrolled account.
			lease, err := pool.AcquireAccount(context.Background(), account.ID)
			if err != nil {
				t.Fatal(err)
			}
			config := account.Config
			config.Enabled = false
			if err := lease.SaveConfig(config); err != nil {
				t.Fatal(err)
			}
			_ = lease.Release()
			reloaded, err := loadAccount(account.Directory)
			if err != nil || reloaded.State != AccountDisabled {
				t.Fatalf("disabled restart: %v %v", reloaded, err)
			}
			config.Enabled = true
			if err := writeAccountConfig(account.ConfigPath, config); err != nil {
				t.Fatal(err)
			}
			reloaded, err = loadAccount(account.Directory)
			if err != nil || reloaded.State != AccountAuthRequired {
				t.Fatalf("restart lost quarantine: %v %v", reloaded, err)
			}
		})
	}
}

func TestRemoteQuarantinePublishedBeforeLeaseRelease(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, nil)
	first, err := pool.Acquire(context.Background(), "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	end, ok := first.BeginAuthRefresh()
	if !ok {
		t.Fatal("generation cannot renew")
	}
	if err := first.UpdateRemoteSession(context.Background(), func(meta *SessionState) error {
		quarantinePending(meta.Remote)
		meta.Remote.Pending.StateRevision = meta.Revision + 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if pool.Status()[0].State != AccountBusy {
		t.Fatal("normal in-progress authentication was reported failed")
	}
	if lease, waitable, err := pool.TryAcquireFor(context.Background(), AccountSelection{AccountID: account.ID}); lease != nil || !waitable || err != nil {
		t.Fatalf("live renewal: lease=%v waitable=%v err=%v", lease, waitable, err)
	}
	end()
	if err := first.RefreshRemoteQuarantine(); err != nil {
		t.Fatal(err)
	}
	// The original safe lease remains live, but even the spare slot is blocked.
	assertNoRemoteGeneration(t, pool, account)
	if err := first.MarkAuthenticationValid(); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkReady(account.ID); err != nil {
		t.Fatal(err)
	}
	assertNoRemoteGeneration(t, pool, account)
	_ = first.Release()
	assertNoRemoteGeneration(t, pool, account)
	healthy := &Account{ID: "healthy@example.invalid", Config: AccountConfig{Enabled: true}, State: AccountReady, Models: account.Models}
	if err := pool.Add(healthy); err != nil {
		t.Fatal(err)
	}
	next, err := pool.Acquire(context.Background(), "fixture-model")
	if err != nil || next.Account() != healthy {
		t.Fatalf("healthy account was blocked: %v", err)
	}
	_ = next.Release()
}

func TestRemoteMaintenanceCompletesOnlyClosedCurrentProjection(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, quarantinePending)
	lease, err := pool.AcquireRemoteMaintenance(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !RemoteAuthenticationAllowed(ContextWithAccountLease(context.Background(), lease)) {
		t.Fatal("explicit maintenance was disabled")
	}
	if err := lease.CompleteRemoteRecovery(); !IsSessionFailureCode(err, "PENDING_RECOVERY") {
		t.Fatalf("pending was cleared: %v", err)
	}
	if err := lease.UpdateRemoteSession(context.Background(), func(meta *SessionState) error {
		meta.Remote.Pending = nil
		meta.Remote.RecoveryRequired = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := lease.CompleteRemoteRecovery(); !IsSessionFailureCode(err, "RECOVERY_REQUIRED") {
		t.Fatalf("retirement was treated as recovery: %v", err)
	}
	if err := lease.RefreshManagedStorageState(context.Background(), func(state *StorageState) error {
		meta, _ := state.Session()
		meta.Remote.RecoveryRequired = false
		meta.Remote.CookieRevision++
		meta.Remote.Ack = &SessionAck{ID: "fixture_ack_000001", Epoch: meta.Remote.Epoch, Cookie: meta.Remote.CookieRevision, Material: meta.Remote.MaterialVersion}
		return state.SetSession(meta)
	}, func() (func(bool), error) { return func(bool) {}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := lease.CompleteRemoteRecovery(); err != nil {
		t.Fatal(err)
	}
	if status := pool.Status()[0]; status.State != AccountBusy || !status.Enabled {
		t.Fatalf("valid committed projection still isolated: %+v", status)
	}
}

func TestRemoteResourceLeaseCannotAuthenticateGenerateOrClear(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, quarantinePending)
	if err := pool.BindResource("fixture-file", account.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireResource(context.Background(), "fixture-file")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx := ContextWithAccountLease(context.Background(), lease)
	if RemoteAuthenticationAllowed(ctx) {
		t.Fatal("resource lease allowed renewal")
	}
	if _, ok := lease.BeginAuthRefresh(); ok {
		t.Fatal("resource lease entered auth refresh")
	}
	for _, selection := range []AccountSelection{{AccountID: account.ID}, {ModelID: "fixture-model", ResourceID: "fixture-file"}} {
		if _, _, err := resolveAccountLease(ctx, pool, selection); !IsSessionFailureCode(err, "LEASE_PURPOSE_MISMATCH") {
			t.Fatalf("resource lease used for new work: %v", err)
		}
	}
	if err := lease.MarkAuthenticationValid(); err != nil {
		t.Fatal(err)
	}
	if err := lease.CompleteRemoteRecovery(); !IsSessionFailureCode(err, "RECOVERY_NOT_ALLOWED") {
		t.Fatalf("resource success completed recovery: %v", err)
	}
	assertNoRemoteGeneration(t, pool, account)
}

type remoteResultTransport struct {
	t     *testing.T
	calls int
}

func (f *remoteResultTransport) Do(ctx context.Context, rpc RPCRequest) (*RPCResponse, error) {
	f.t.Helper()
	if RemoteAuthenticationAllowed(ctx) {
		f.t.Fatal("result operation permitted automatic authentication")
	}
	f.calls++
	body := "[false]"
	if rpc.Method == "GenerateAccessToken" {
		body = "[\"synthetic-token\"]"
	}
	return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func (f *remoteResultTransport) UploadDrive(context.Context, string, string, UploadRequest) (FileRef, error) {
	f.t.Fatal("result operation started upload")
	return FileRef{}, nil
}
func (f *remoteResultTransport) DownloadDrive(ctx context.Context, _, _, _ string) (MediaStream, error) {
	if RemoteAuthenticationAllowed(ctx) {
		f.t.Fatal("download permitted authentication")
	}
	return MediaStream{Body: io.NopCloser(strings.NewReader("synthetic-result"))}, nil
}
func (f *remoteResultTransport) DeleteDrive(ctx context.Context, _, _, _ string) error {
	if RemoteAuthenticationAllowed(ctx) {
		f.t.Fatal("cleanup permitted authentication")
	}
	return nil
}

func TestRemoteResourceServiceResultsSurviveQuarantine(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, quarantinePending)
	if _, err := pool.bindVideoOperation(context.Background(), "fixture-operation", account.ID, VideoResourceMetadata{Model: "fixture-model", Seconds: "4", Size: "1280x720"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.BindResource("fixture-file", account.ID); err != nil {
		t.Fatal(err)
	}
	transport := &remoteResultTransport{t: t}
	client := &Client{transport: transport}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetGenerateVideoOperation(context.Background(), "fixture-operation"); err != nil {
		t.Fatal(err)
	}
	media, err := service.DownloadFile(context.Background(), "fixture-file")
	if err != nil {
		t.Fatal(err)
	}
	_ = media.Body.Close()
	if err := service.deleteDriveResource(context.Background(), "fixture-file"); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 3 || pool.Status()[0].State != AccountAuthRequired {
		t.Fatalf("results changed quarantine or unexpected RPCs: calls=%d status=%+v", transport.calls, pool.Status())
	}
}

func TestRemoteQuarantineCanceledBusyDoesNotFailHealthyAccount(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, nil)
	lease, err := pool.Acquire(context.Background(), "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	end, ok := lease.BeginAuthRefresh()
	if !ok {
		t.Fatal("refresh unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.AcquireFor(ctx, AccountSelection{AccountID: account.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
	end()
	_ = lease.Release()
	if pool.Status()[0].State != AccountReady {
		t.Fatal("busy/cancellation quarantined a healthy account")
	}
}

func TestRemoteQuarantineDiskFenceCatchesStalePool(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, nil)
	state, err := LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	release, err := AcquireSessionAccountLease(context.Background(), account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := state.Session()
	quarantinePending(meta.Remote)
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	if err := WriteManagedSessionState(context.Background(), account.StoragePath, &state); err != nil {
		t.Fatal(err)
	}
	release()
	// The in-memory account is still healthy: the real file-lease fence must win.
	if lease, _, err := pool.TryAcquireFor(context.Background(), AccountSelection{AccountID: account.ID}); lease != nil || err != nil {
		t.Fatalf("stale pool acquired isolated file: lease=%v err=%v", lease, err)
	}
	if err := sessionAcquisitionFence(account.StoragePath); !IsSessionFailureCode(err, "PENDING_RECOVERY") {
		t.Fatalf("file fence: %v", err)
	}
	maintenance, err := pool.AcquireRemoteMaintenance(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = maintenance.Release()
	assertNoRemoteGeneration(t, pool, account)
}

func TestRemoteMaintenanceControlReadKeepsSessionRevisionAndLock(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, quarantinePending)
	lease, err := pool.AcquireRemoteMaintenance(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := before.Session()
	if err := lease.WithRemoteSession(context.Background(), func(current *SessionState) error {
		if current.Revision != meta.Revision {
			t.Fatal("control read changed revision")
		}
		lock, _, err := acquireAccountFileLeasePurpose(account.StoragePath, leaseManagement)
		if lock != nil || !errors.Is(err, ErrAccountLeased) {
			t.Fatal("control callback did not retain real account lock")
		}
		current.Remote.Pending = nil // Caller receives detached metadata only.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := after.Session()
	if actual.Revision != meta.Revision || actual.Remote.Pending == nil {
		t.Fatal("control read wrote the authentication document")
	}
	_ = lease.Release()
	if err := lease.WithRemoteSession(context.Background(), func(*SessionState) error { t.Fatal("released lease callback"); return nil }); !IsSessionFailureCode(err, "CANCELED") {
		t.Fatalf("released callback: %v", err)
	}
}

func TestRemoteQuarantineBackgroundCatalogCannotUseMaintenance(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "disabled"}[disabled], func(t *testing.T) {
			pool, account := remoteQuarantineFixture(t, quarantinePending)
			if disabled {
				lease, err := pool.AcquireAccount(t.Context(), account.ID)
				if err != nil {
					t.Fatal(err)
				}
				config := account.Config
				config.Enabled = false
				if err := lease.SaveConfig(config); err != nil {
					t.Fatal(err)
				}
				lease.Release()
			}
			service, err := NewPooledService(pool, &Client{})
			if err != nil {
				t.Fatal(err)
			}
			// A nil transport would panic if the ordinary directory path reached RPC.
			if _, err := service.RefreshAccountModels(t.Context(), account.ID); !errors.Is(err, ErrNoEligibleAccount) {
				t.Fatal("background catalog bypassed generation fence", err)
			}
			maintenance, err := pool.AcquireRemoteMaintenance(t.Context(), account.ID)
			if err != nil {
				t.Fatal("explicit maintenance blocked", err)
			}
			maintenance.Release()
		})
	}
}

func TestRemoteQuarantineDisabledCompletionPreservesIntent(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, nil)
	lease, err := pool.AcquireRemoteMaintenance(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	config := account.Config
	config.Enabled = false
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := lease.CompleteRemoteRecovery(); err != nil {
		t.Fatal(err)
	}
	if status := pool.Status()[0]; status.State != AccountDisabled || status.Enabled {
		t.Fatalf("automatic completion re-enabled account: %+v", status)
	}
	_ = lease.Release()
}

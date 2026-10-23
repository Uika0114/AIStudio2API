package aistudio

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func sessionTestState(t *testing.T, authority bool) StorageState {
	t.Helper()
	state := StorageState{Cookies: []StateCookie{{Name: "SAPISID", Value: "fixture", Domain: ".google.com", Path: "/", Expires: -1, Secure: true}}}
	meta := &SessionState{Version: 1}
	if authority {
		meta.Authority = &SessionAuthority{Handle: "fixture_handle_0001", Consumer: "consumer.fixture.invalid", GaiaID: "fixture_gaia", Email: "fixture@example.invalid", Epoch: 1, MaterialVersion: 1, Enabled: true}
	} else {
		meta.Remote = &RemoteSession{Handle: "fixture_handle_0001", Consumer: "consumer.fixture.invalid", GaiaID: "fixture_gaia", Email: "fixture@example.invalid", Epoch: 1, MaterialVersion: 1}
	}
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSessionAuthorityFencesOrdinaryWritersAndAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture", "storage-state.json")
	state := sessionTestState(t, true)
	if err := WriteManagedSessionState(context.Background(), path, &state); err != nil {
		t.Fatal(err)
	}
	if err := WriteStorageState(path, state); !IsSessionFailure(err) {
		t.Fatalf("ordinary authority write: %v", err)
	}
	lock, _, err := acquireAccountFileLease(path)
	if lock != nil || !IsSessionFailure(err) {
		t.Fatal("generation acquired authority")
	}
	release, err := AcquireSessionAccountLease(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := AcquireSessionAccountLease(ctx, path); !IsSessionFailure(err) {
		t.Fatal("concurrent helper was not excluded")
	}
}

func TestSessionCookieWritersIncrementPersistentRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture", "storage-state.json")
	state := sessionTestState(t, false)
	if err := WriteManagedSessionState(context.Background(), path, &state); err != nil {
		t.Fatal(err)
	}
	original, _ := state.Session()
	state.Cookies[0].Value = "changed-fixture"
	if err := WriteStorageState(path, state); err != nil {
		t.Fatal(err)
	}
	actual, err := LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := actual.Session()
	if meta.Revision != original.Revision+1 || actual.Cookies[0].Value != "changed-fixture" {
		t.Fatal("cookie writer did not version")
	}
	if err := WriteStorageState(path, state); !IsSessionFailure(err) {
		t.Fatal("stale cookie write accepted")
	}
	if err := WriteStorageState(path, StorageState{}); !IsSessionFailure(err) {
		t.Fatal("binding silently discarded")
	}
}

func TestSessionCASSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture", "storage-state.json")
	state := sessionTestState(t, false)
	if err := WriteManagedSessionState(context.Background(), path, &state); err != nil {
		t.Fatal(err)
	}
	a, _ := LoadStorageState(path)
	b, _ := LoadStorageState(path)
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, s := range []StorageState{a, b} {
		group.Add(1)
		go func(value StorageState) {
			defer group.Done()
			<-start
			results <- WriteManagedSessionState(context.Background(), path, &value)
		}(s)
	}
	close(start)
	group.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if IsSessionFailure(err) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("CAS: %d %d", success, conflict)
	}
}

func TestSessionRejectsLocalMaterialOnConsumer(t *testing.T) {
	state := sessionTestState(t, false)
	if err := state.SetAuthExtension(AuthExtension{Source: AuthSource{Browser: "chrome", ChromeRoot: "D:/fixture-only"}, OAuth: &ChromeOAuthMaterial{GaiaID: "fixture_gaia", RefreshToken: "not-a-token"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Session(); !IsSessionFailure(err) {
		t.Fatal("local material leaked to consumer")
	}
	wrapped := errors.Join(errors.New("fixture network failure"), SessionError("HELPER_UNAVAILABLE"))
	if !IsSessionFailure(wrapped) {
		t.Fatal("joined recovery failure not detected")
	}
}

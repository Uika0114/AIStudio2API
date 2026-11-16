package app

import (
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestWorkerWebAccessStartIdentityAndFailure(t *testing.T) {
	account := &accountWorker{id: "fixture"}
	manager := &accountWorkerManager{accounts: map[string]*accountWorker{"fixture": account}}
	old, finishOld := manager.workerPageObserver(account)
	now := time.Now().UTC()
	old(aistudio.WebAccessCheck{Access: aistudio.WebAccessRestricted, CheckedAt: now})
	finishOld()
	old(aistudio.WebAccessCheck{Access: aistudio.WebAccessNormal, CheckedAt: now.Add(time.Hour)})
	if account.webAccess.Access != aistudio.WebAccessRestricted {
		t.Fatal("finished start overwrote evidence")
	}
	current, finishCurrent := manager.workerPageObserver(account)
	defer finishCurrent()
	current(aistudio.WebAccessCheck{Access: aistudio.WebAccessUnknown, CheckedAt: now.Add(time.Minute)})
	if account.webAccess.Access != aistudio.WebAccessRestricted || !account.webAccess.ObservedAt.Equal(now) || account.webAccess.LastCheck.Access != aistudio.WebAccessUnknown {
		t.Fatalf("failed check erased evidence: %+v", account.webAccess)
	}
	old(aistudio.WebAccessCheck{Access: aistudio.WebAccessNormal, CheckedAt: now.Add(2 * time.Hour)})
	finishOld()
	current(aistudio.WebAccessCheck{Access: aistudio.WebAccessNormal, CheckedAt: now.Add(2 * time.Minute)})
	if account.webAccess.Access != aistudio.WebAccessNormal {
		t.Fatal("old finish invalidated current start")
	}
	if account.generation.Load() != 0 {
		t.Fatal("observation mutated WorkerGeneration")
	}
}

func TestWorkerWebAccessReplacementAndConfiguration(t *testing.T) {
	account := &accountWorker{id: "fixture"}
	manager := &accountWorkerManager{accounts: map[string]*accountWorker{"fixture": account}}
	observe, finish := manager.workerPageObserver(account)
	defer finish()
	observe(aistudio.WebAccessCheck{Access: aistudio.WebAccessRestricted, CheckedAt: time.Now()})
	account.startupMu.Lock()
	update := &accountWorkerUpdate{account: account, pending: true, label: "same-id-new-config"}
	update.Commit()
	observe(aistudio.WebAccessCheck{Access: aistudio.WebAccessNormal, CheckedAt: time.Now().Add(time.Minute)})
	if account.webAccess.Access != "" || !account.webAccess.ObservedAt.IsZero() {
		t.Fatal("configuration change inherited old observation")
	}
	current, finishCurrent := manager.workerPageObserver(account)
	defer finishCurrent()
	replacement := &accountWorker{id: "fixture"}
	manager.mu.Lock()
	manager.accounts["fixture"] = replacement
	manager.mu.Unlock()
	current(aistudio.WebAccessCheck{Access: aistudio.WebAccessRestricted, CheckedAt: time.Now()})
	if replacement.webAccess.Access != "" || account.webAccess.Access != "" {
		t.Fatal("same ID accepted old object's observation")
	}
}

func TestWorkerWebAccessConcurrentCallbacksAndClosure(t *testing.T) {
	account := &accountWorker{id: "fixture"}
	manager := &accountWorkerManager{accounts: map[string]*accountWorker{"fixture": account}}
	observe, finish := manager.workerPageObserver(account)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			observe(aistudio.WebAccessCheck{Access: aistudio.WebAccessRestricted, CheckedAt: time.Now()})
		}()
	}
	wg.Wait()
	finish()
	before := account.webAccess
	observe(aistudio.WebAccessCheck{Access: aistudio.WebAccessNormal, CheckedAt: time.Now().Add(time.Minute)})
	if account.webAccess != before {
		t.Fatal("post-close callback applied")
	}
	latest, done := manager.workerPageObserver(account)
	defer done()
	manager.mu.Lock()
	manager.closed = true
	manager.mu.Unlock()
	latest(aistudio.WebAccessCheck{Access: aistudio.WebAccessNormal, CheckedAt: time.Now().Add(time.Hour)})
	if account.webAccess != before {
		t.Fatal("closed manager accepted callback")
	}
}

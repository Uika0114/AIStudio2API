package app

import (
	"context"
	"fmt"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// observeWorkerPage scopes observations to this account object and this exact
// startup, including failed first starts (which have no WorkerGeneration yet).
// It does not mutate the pool's authentication, enabled or dispatch state.
func (manager *accountWorkerManager) observeWorkerPage(ctx context.Context, account *accountWorker) (context.Context, func()) {
	observe, finish := manager.workerPageObserver(account)
	return aistudio.ContextWithWebAccessObserver(ctx, observe), finish
}

func (manager *accountWorkerManager) workerPageObserver(account *accountWorker) (func(aistudio.WebAccessCheck), func()) {
	account.mu.Lock()
	account.webAccessToken++
	token := account.webAccessToken
	account.mu.Unlock()
	observed := func(check aistudio.WebAccessCheck) {
		account.mu.Lock()
		manager.mu.RLock()
		current := !manager.closed && manager.accounts[account.id] == account
		manager.mu.RUnlock()
		if !current || account.webAccessToken != token {
			account.mu.Unlock()
			return
		}
		account.webAccess.Observe(check)
		outcome, label := account.webAccess.LastCheck.Access, account.label
		account.mu.Unlock()
		if manager.requests != nil {
			manager.requests.log(label, "INFO", fmt.Sprintf("WAA 网页观察 | 结果=%s", outcome))
		}
	}
	return observed, func() {
		account.mu.Lock()
		defer account.mu.Unlock()
		if account.webAccessToken == token {
			account.webAccessToken++
		}
	}
}

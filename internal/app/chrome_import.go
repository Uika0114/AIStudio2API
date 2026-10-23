package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

type chromeAccountProvider interface {
	Discover(string) ([]chromeauth.Account, error)
	Import(context.Context, chromeauth.ImportOptions) ([]chromeauth.ImportResult, error)
}

type nativeChromeAccountProvider struct{}

func (nativeChromeAccountProvider) Discover(root string) ([]chromeauth.Account, error) {
	return chromeauth.Discover(root)
}
func (nativeChromeAccountProvider) Import(ctx context.Context, options chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
	return chromeauth.Import(ctx, options)
}
func (admin *runtimeAdmin) chromeProvider() chromeAccountProvider {
	if admin.chrome != nil {
		return admin.chrome
	}
	return nativeChromeAccountProvider{}
}

func (admin *runtimeAdmin) ChromeImportProfilesFrom(ctx context.Context, inputRoot string) (api.ChromeImportDiscovery, error) {
	if err := ctx.Err(); err != nil {
		return api.ChromeImportDiscovery{}, err
	}
	root, err := chromeauth.NormalizeChromeRoot(inputRoot)
	if err != nil {
		return api.ChromeImportDiscovery{}, invalidAccount(err)
	}
	accounts, err := admin.chromeProvider().Discover(root)
	if err != nil {
		return api.ChromeImportDiscovery{}, err
	}
	existing := map[string]string{}
	for _, status := range admin.pool.Status() {
		existing[strings.ToLower(strings.TrimSpace(status.ID))] = status.ID
	}
	profiles := make([]api.ChromeImportProfile, 0, len(accounts))
	for _, account := range accounts {
		if account.ChromeRoot == "" {
			return api.ChromeImportDiscovery{}, fmt.Errorf("Chrome 发现结果缺少来源根")
		}
		actual, err := chromeauth.NormalizeChromeRoot(account.ChromeRoot)
		if err != nil || !strings.EqualFold(actual, root) {
			return api.ChromeImportDiscovery{}, fmt.Errorf("Chrome 发现结果不属于指定根")
		}
		email := strings.ToLower(strings.TrimSpace(account.Email))
		profiles = append(profiles, api.ChromeImportProfile{ID: account.ID, ChromeRoot: root, Profile: account.Profile, DisplayName: account.DisplayName, Email: email, Locale: account.Locale, Importable: account.Importable, TokenFormat: account.TokenFormat, Reason: account.Reason, ExistingAccountID: existing[email]})
	}
	return api.ChromeImportDiscovery{ChromeRoot: root, Profiles: profiles}, nil
}

func selectedChromeAccounts(root string, accounts []chromeauth.Account, ids []string, legacyDefault bool) ([]chromeauth.Account, error) {
	if len(ids) == 0 {
		return nil, invalidAccount(fmt.Errorf("未选择 Chrome 账号"))
	}
	requested := map[string]bool{}
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" || requested[id] {
			return nil, invalidAccount(fmt.Errorf("Chrome 选择为空或重复"))
		}
		requested[id] = true
	}
	selected := make([]chromeauth.Account, 0, len(ids))
	found := map[string]bool{}
	emails := map[string]bool{}
	for _, account := range accounts {
		id := strings.ToLower(account.ID)
		legacy := strings.ToLower(account.Profile + "/" + account.GaiaID)
		match := id
		if !requested[id] {
			if !legacyDefault || !requested[legacy] {
				continue
			}
			match = legacy
		}
		if account.ChromeRoot == "" {
			return nil, invalidAccount(fmt.Errorf("Chrome 选择缺少来源根"))
		}
		actual, err := chromeauth.NormalizeChromeRoot(account.ChromeRoot)
		if account.ChromeRoot == "" || err != nil || !strings.EqualFold(actual, root) {
			return nil, invalidAccount(fmt.Errorf("Chrome 选择属于其他根"))
		}
		if !account.Importable || account.GaiaID == "" || !strings.Contains(account.Email, "@") {
			return nil, invalidAccount(fmt.Errorf("所选 Chrome 身份没有可导入的设备绑定材料"))
		}
		email := strings.ToLower(strings.TrimSpace(account.Email))
		if emails[email] || found[match] {
			return nil, invalidAccount(fmt.Errorf("同一账号存在多个来源，请精确选择一个"))
		}
		emails[email] = true
		found[match] = true
		account.ChromeRoot = root
		selected = append(selected, account)
	}
	if len(found) != len(requested) {
		return nil, invalidAccount(fmt.Errorf("Chrome 选择已失效或不属于指定根，请重新发现并选择"))
	}
	return selected, nil
}

func validateChromeResult(account chromeauth.Account, result chromeauth.ImportResult) error {
	ext, exists, err := result.State.AuthExtension()
	expected := aistudio.AuthSource{Browser: "chrome", ChromeRoot: account.ChromeRoot, Profile: account.Profile, GaiaID: account.GaiaID, Email: strings.ToLower(strings.TrimSpace(account.Email))}
	if err != nil || !exists || ext.OAuth == nil || ext.OAuth.GaiaID != account.GaiaID || !chromeauth.SameSource(expected, ext.Source) || !strings.EqualFold(result.Email, expected.Email) {
		return fmt.Errorf("Chrome 导入结果与选定来源不匹配")
	}
	if _, err := aistudio.NewSigner().Sign(result.State); err != nil {
		return fmt.Errorf("Chrome 认证状态无法用于 AI Studio: %w", err)
	}
	return nil
}

func (admin *runtimeAdmin) importChromeFrom(ctx context.Context, input api.ChromeImportInput) ([]api.AdminAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input.RebindAccountID = strings.TrimSpace(input.RebindAccountID)
	if len(input.AccountIDs) == 0 {
		return nil, invalidAccount(fmt.Errorf("未选择 Chrome 账号"))
	}
	if input.RebindAccountID != "" && (len(input.AccountIDs) != 1 || input.Proxy != "" || input.Locale != "" || input.Timezone != "") {
		return nil, invalidAccount(fmt.Errorf("来源重绑仅接受一个身份，不修改账号环境"))
	}
	if input.RebindAccountID != "" && !input.ConfirmRebind || input.RebindAccountID == "" && input.ConfirmRebind {
		return nil, invalidAccount(fmt.Errorf("来源重绑需要单一目标和显式确认"))
	}
	root, err := chromeauth.NormalizeChromeRoot(input.ChromeRoot)
	if err != nil {
		return nil, invalidAccount(err)
	}
	accounts, err := admin.chromeProvider().Discover(root)
	if err != nil {
		return nil, err
	}
	selected, err := selectedChromeAccounts(root, accounts, input.AccountIDs, strings.TrimSpace(input.ChromeRoot) == "")
	if err != nil {
		return nil, err
	}
	if input.RebindAccountID != "" {
		if len(selected) != 1 {
			return nil, invalidAccount(fmt.Errorf("来源重绑只允许一个 Chrome 身份"))
		}
		if input.Proxy != "" || input.Locale != "" || input.Timezone != "" {
			return nil, invalidAccount(fmt.Errorf("来源重绑不修改代理、语言或时区，请勿混入这些设置"))
		}
		account, err := admin.rebindChromeSource(ctx, input.RebindAccountID, selected[0])
		if err != nil {
			return nil, err
		}
		return []api.AdminAccount{account}, nil
	}
	existing := map[string]bool{}
	for _, status := range admin.pool.Status() {
		existing[strings.ToLower(status.ID)] = true
	}
	ids := make([]string, len(selected))
	expected := map[string]chromeauth.Account{}
	for i, account := range selected {
		email := strings.ToLower(strings.TrimSpace(account.Email))
		if existing[email] {
			return nil, &adminOperationError{status: http.StatusConflict, code: "account_exists", message: "账号已存在；如需更换 Chrome 来源，请显式重绑"}
		}
		ids[i] = account.ID
		expected[email] = account
	}
	accountProxy := strings.TrimSpace(input.Proxy)
	results, err := admin.chromeProvider().Import(ctx, chromeauth.ImportOptions{ChromeRoot: root, Proxy: admin.effectiveProxy(accountProxy), AccountIDs: ids})
	if err != nil {
		return nil, err
	}
	if len(results) != len(selected) {
		return nil, fmt.Errorf("Chrome 导入返回数量与选择不匹配")
	}
	configs := make([]aistudio.AccountConfig, len(results))
	seen := map[string]bool{}
	for i, result := range results {
		email := strings.ToLower(strings.TrimSpace(result.Email))
		account, ok := expected[email]
		if !ok || seen[email] {
			return nil, fmt.Errorf("Chrome 导入返回重复或未选定的身份")
		}
		seen[email] = true
		if err := validateChromeResult(account, result); err != nil {
			return nil, err
		}
		cfg := aistudio.DefaultAccountConfig(email)
		cfg.Proxy = accountProxy
		if input.Locale != "" {
			cfg.Locale = strings.TrimSpace(input.Locale)
		} else if result.Locale != "" {
			cfg.Locale = strings.TrimSpace(result.Locale)
		}
		if input.Timezone != "" {
			cfg.Timezone = strings.TrimSpace(input.Timezone)
		}
		if err := cfg.Validate(); err != nil {
			return nil, invalidAccount(err)
		}
		configs[i] = cfg
	}
	created := make([]api.AdminAccount, 0, len(results))
	for i, result := range results {
		var account api.AdminAccount
		if err = ctx.Err(); err == nil {
			if admin.chromeCreate != nil {
				account, err = admin.chromeCreate(ctx, configs[i], result.State)
			} else {
				account, err = admin.addAccount(ctx, configs[i], result.State, "")
			}
		}
		if err != nil {
			if len(created) != 0 {
				return created, &adminOperationError{status: http.StatusConflict, code: "chrome_import_partial", message: fmt.Sprintf("已导入 %d 个账号，后续失败；未自动回滚，请刷新账号列表: %v", len(created), err)}
			}
			return nil, err
		}
		created = append(created, account)
		admin.requests.log("auth", "INFO", "Chrome 账户已导入 | 账户="+account.Label)
	}
	return created, nil
}

func (admin *runtimeAdmin) rebindChromeSource(ctx context.Context, accountID string, selected chromeauth.Account) (updated api.AdminAccount, resultErr error) {
	if !strings.EqualFold(strings.TrimSpace(accountID), strings.TrimSpace(selected.Email)) {
		return api.AdminAccount{}, invalidAccount(fmt.Errorf("重绑目标与 Chrome 邮箱不匹配"))
	}
	if busy, _ := admin.pool.Activity(accountID); busy {
		return api.AdminAccount{}, &adminOperationError{status: http.StatusConflict, code: "account_busy", message: "账号正在使用，不能重绑 Chrome 来源"}
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	lease, err := admin.pool.AcquireAccount(acquireCtx, accountID)
	cancel()
	if err != nil {
		return api.AdminAccount{}, accountOperationError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Release()) }()
	account := lease.Account()
	err = lease.RefreshStorageState(func(state *aistudio.StorageState) error {
		if err := ctx.Err(); err != nil {
			if session, sessionErr := state.Session(); sessionErr != nil || session != nil {
				return aistudio.SessionError("REMOTE_OWNED")
			}
			return err
		}
		old, exists, err := state.AuthExtension()
		if err != nil {
			return err
		}
		knownGaia := ""
		if exists {
			knownGaia = old.Source.GaiaID
			if old.OAuth != nil {
				if knownGaia != "" && knownGaia != old.OAuth.GaiaID {
					return fmt.Errorf("旧来源与保存材料的 Gaia 身份冲突，不能自动重绑")
				}
				knownGaia = old.OAuth.GaiaID
			}
		}
		if knownGaia != "" && knownGaia != selected.GaiaID {
			return fmt.Errorf("重绑来源的 Gaia 身份与原账号不同")
		}
		expectedSource := aistudio.AuthSource{Browser: "chrome", ChromeRoot: selected.ChromeRoot, Profile: selected.Profile, GaiaID: selected.GaiaID, Email: strings.ToLower(strings.TrimSpace(selected.Email))}
		results, err := admin.chromeProvider().Import(ctx, chromeauth.ImportOptions{ChromeRoot: selected.ChromeRoot, ExactSource: &expectedSource, Proxy: account.EffectiveProxy(admin.globalProxy)})
		if err != nil {
			return err
		}
		if len(results) != 1 {
			return fmt.Errorf("来源重绑没有返回单一账号")
		}
		if err := validateChromeResult(selected, results[0]); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		*state = results[0].State
		return nil
	}, func() (func(bool), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if admin.chromePrepareRebind != nil {
			return admin.chromePrepareRebind(account.ID)
		}
		if admin.workers == nil || admin.headers == nil {
			return nil, fmt.Errorf("Chrome 来源重绑运行时不可用")
		}
		if err := admin.workers.Reset(account.ID); err != nil {
			return nil, err
		}
		return admin.headers.prepareInvalidate(account.ID)
	})
	if err != nil {
		return api.AdminAccount{}, err
	}
	// MarkReady does not clear quota/cooldown or model catalogs; disabled stays disabled.
	if err := admin.pool.MarkReady(account.ID); err != nil {
		return api.AdminAccount{}, err
	}
	admin.requests.log("auth", "INFO", "Chrome 来源已绑定 | 账户="+account.ID)
	statuses, err := admin.Accounts(ctx)
	if err == nil {
		admin.requests.publish(api.AdminEvent{Type: "accounts", Data: map[string]any{"accounts": statuses}})
	}
	return admin.account(account.ID)
}

var _ api.ChromeSourceAdmin = (*runtimeAdmin)(nil)
var _ chromeAccountProvider = nativeChromeAccountProvider{}

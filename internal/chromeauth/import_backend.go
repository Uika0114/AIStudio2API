package chromeauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// importBackend is explicit: a fixture never falls back to native key or HTTP calls.
type importBackend struct {
	discover  func(string, string) ([]Account, error)
	readToken func(string, string, string) (string, []byte, []byte, error)
	key       func(string, string) ([]byte, error)
	cookies   func(context.Context, string, string, []byte, string) ([]multiloginCookie, error)
}

func nativeImportBackend() importBackend {
	return importBackend{
		discover: discoverPlatformProfiles, readToken: readTokenService,
		key: func(root, format string) ([]byte, error) {
			switch format {
			case "v10":
				return retrieveV10Key(root)
			case "v20":
				return retrieveV20Key(root)
			default:
				return nil, fmt.Errorf("不支持的 Chrome OAuth 密文格式")
			}
		},
		cookies: fetchGoogleCookies,
	}
}

func tokenFormat(encrypted []byte) (string, error) {
	if len(encrypted) < 3+12+16 {
		return "", fmt.Errorf("Chrome OAuth 密文长度异常")
	}
	format := string(encrypted[:3])
	if format != "v10" && format != "v20" {
		return "", fmt.Errorf("不支持的 Chrome OAuth 密文格式")
	}
	return format, nil
}

func importWithBackend(ctx context.Context, options ImportOptions, backend importBackend) ([]ImportResult, error) {
	if backend.discover == nil || backend.readToken == nil || backend.key == nil || backend.cookies == nil {
		return nil, fmt.Errorf("Chrome 导入后端不完整")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proxy, err := validateProxy(options.Proxy)
	if err != nil {
		return nil, err
	}
	var exact *aistudio.AuthSource
	rootInput := options.ChromeRoot
	if options.ExactSource != nil {
		source, err := ValidateSource(*options.ExactSource, nil)
		if err != nil {
			return nil, err
		}
		if rootInput != "" {
			root, err := NormalizeChromeRoot(rootInput)
			if err != nil || !sameChromeRoot(root, source.ChromeRoot) {
				return nil, fmt.Errorf("Chrome 导入根与绑定来源不匹配")
			}
		}
		rootInput, exact = source.ChromeRoot, &source
	}
	root, err := NormalizeChromeRoot(rootInput)
	if err != nil {
		return nil, err
	}
	profile := ""
	if exact != nil {
		profile = exact.Profile
	}
	if exact != nil && (len(options.AccountIDs) != 0 || len(options.Profiles) != 0 || len(options.Emails) != 0) {
		return nil, fmt.Errorf("精确来源恢复不能混入其他账号选择")
	}
	prefix := strings.SplitN(sourceAccountID(root, "", ""), "/", 2)[0] + "/"
	for _, id := range options.AccountIDs {
		if strings.HasPrefix(strings.ToLower(id), "chrome-") {
			if !strings.HasPrefix(strings.ToLower(id), strings.ToLower(prefix)) {
				return nil, fmt.Errorf("Chrome 选择属于其他根，请重新发现并选择")
			}
		} else if options.ChromeRoot != "" {
			return nil, fmt.Errorf("Chrome 选择缺少指定根的来源标识")
		}
	}
	accounts, err := backend.discover(root, profile)
	if err != nil {
		return nil, err
	}
	ids := append([]string(nil), options.AccountIDs...)
	// Only the explicit legacy default-import API may use old, unscoped IDs.
	if options.ChromeRoot == "" && exact == nil {
		for i, id := range ids {
			if strings.HasPrefix(id, "chrome-") {
				continue
			}
			for _, account := range accounts {
				if strings.EqualFold(id, account.Profile+"/"+account.GaiaID) {
					ids[i] = account.ID
					break
				}
			}
		}
	}
	if exact != nil {
		ids = []string{sourceAccountID(root, exact.Profile, exact.GaiaID)}
	}
	selected, err := selectAccounts(accounts, options.Profiles, options.Emails, ids)
	if err != nil {
		return nil, err
	}
	keys := make(map[string][]byte)
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	results := make([]ImportResult, 0, len(selected))
	for _, account := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !sameChromeRoot(account.ChromeRoot, root) || account.ID != sourceAccountID(root, account.Profile, account.GaiaID) {
			return nil, fmt.Errorf("Chrome 选择不属于指定根，请重新发现并选择")
		}
		if exact != nil && (!strings.EqualFold(account.Profile, exact.Profile) || account.GaiaID != exact.GaiaID || !strings.EqualFold(account.Email, exact.Email)) {
			return nil, fmt.Errorf("Chrome 来源身份不匹配")
		}
		result, err := importSelectedAccount(ctx, root, proxy, account, backend, keys)
		if err != nil {
			return nil, fmt.Errorf("导入 %s: %w", account.Profile, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func importSelectedAccount(ctx context.Context, root, proxy string, account Account, backend importBackend, keys map[string][]byte) (ImportResult, error) {
	gaia, encrypted, binding, err := backend.readToken(root, account.Profile, account.GaiaID)
	if err != nil {
		return ImportResult{}, err
	}
	if gaia != account.GaiaID || len(binding) == 0 {
		return ImportResult{}, fmt.Errorf("Chrome 设备绑定身份与选择不匹配")
	}
	format, err := tokenFormat(encrypted)
	if err != nil {
		return ImportResult{}, err
	}
	key := keys[format]
	if key == nil {
		key, err = backend.key(root, format)
		if err != nil {
			return ImportResult{}, err
		}
		if len(key) != 32 {
			clear(key)
			return ImportResult{}, fmt.Errorf("Chrome 主密钥长度异常")
		}
		keys[format] = key
	}
	token, err := decryptChromeToken(key, encrypted)
	if err != nil {
		return ImportResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	cookies, err := backend.cookies(ctx, gaia, token, binding, proxy)
	if err != nil {
		return ImportResult{}, err
	}
	state := aistudio.StorageState{Cookies: toStorageCookies(cookies), Origins: []aistudio.StorageOrigin{}}
	source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: root, Profile: account.Profile, GaiaID: gaia, Email: strings.ToLower(strings.TrimSpace(account.Email))}
	if err := state.SetAuthExtension(aistudio.AuthExtension{
		Source: source,
		OAuth:  &aistudio.ChromeOAuthMaterial{GaiaID: gaia, RefreshToken: token, WrappedBindingKey: append([]byte(nil), binding...)},
	}); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Profile: account.Profile, DisplayName: account.DisplayName, Email: source.Email, Locale: account.Locale, CookieCount: len(cookies), State: state}, nil
}

// SourceAccount inspects only the bound Profile; no other Profile or root is read.
func SourceAccount(source aistudio.AuthSource, material *aistudio.ChromeOAuthMaterial) (Account, error) {
	source, err := ValidateSource(source, material)
	if err != nil {
		return Account{}, err
	}
	accounts, err := discoverPlatformProfiles(source.ChromeRoot, source.Profile)
	if err != nil {
		return Account{}, err
	}
	for _, account := range accounts {
		if strings.EqualFold(account.Profile, source.Profile) && account.GaiaID == source.GaiaID && strings.EqualFold(account.Email, source.Email) && account.Importable {
			return account, nil
		}
	}
	return Account{}, fmt.Errorf("绑定的 Chrome Profile 中没有可导入的精确身份")
}

//go:build windows

package chromeauth

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

func discoverPlatformProfiles(chromeRoot, exactProfile string) ([]Account, error) {
	if strings.TrimSpace(chromeRoot) == "" {
		return nil, ErrSourceBindingRequired
	}
	root, err := NormalizeChromeRoot(chromeRoot)
	if err != nil {
		return nil, err
	}
	if exactProfile != "" {
		if err := validateProfileName(exactProfile); err != nil {
			return nil, err
		}
	}
	statePath, err := checkedChromePath(root, "", "Local State")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("读取 Chrome Local State 失败")
	}
	var state struct {
		Variations struct {
			SafeSeedLocale string `json:"safe_seed_locale"`
		} `json:"variations"`
		Profile struct {
			InfoCache map[string]struct {
				Name     string `json:"name"`
				UserName string `json:"user_name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if json.Unmarshal(data, &state) != nil {
		return nil, fmt.Errorf("解析 Chrome Local State 失败")
	}
	if state.Profile.InfoCache == nil {
		return nil, fmt.Errorf("Chrome Local State 缺少 profile.info_cache")
	}
	profiles := make([]string, 0, len(state.Profile.InfoCache))
	if exactProfile != "" {
		if _, ok := state.Profile.InfoCache[exactProfile]; !ok {
			return nil, fmt.Errorf("绑定的 Chrome Profile 不存在")
		}
		profiles = append(profiles, exactProfile)
	} else {
		for profile := range state.Profile.InfoCache {
			profiles = append(profiles, profile)
		}
		sort.Slice(profiles, func(i, j int) bool { return naturalLess(profiles[i], profiles[j]) })
	}
	accounts := make([]Account, 0, len(profiles))
	for _, profile := range profiles {
		if err := validateProfileName(profile); err != nil {
			return nil, err
		}
		info := state.Profile.InfoCache[profile]
		locale := profileLocale(root, profile)
		if locale == "" {
			locale = state.Variations.SafeSeedLocale
		}
		identities, err := profileIdentities(root, profile)
		if err != nil {
			accounts = append(accounts, Account{ID: sourceAccountID(root, profile, ""), ChromeRoot: root, Profile: profile, DisplayName: info.Name, Email: info.UserName, Locale: locale, Reason: "Profile 没有可读取的 Google 身份"})
			continue
		}
		seen := map[string]bool{}
		for _, identity := range identities {
			if identity.Gaia == "" {
				continue
			}
			if seen[identity.Gaia] {
				return nil, fmt.Errorf("Chrome Profile 含重复的 Gaia 身份")
			}
			seen[identity.Gaia] = true
			_, encrypted, binding, tokenErr := readTokenService(root, profile, identity.Gaia)
			format, reason := "", ""
			if tokenErr == nil {
				format, tokenErr = tokenFormat(encrypted)
			}
			if tokenErr != nil {
				reason = tokenErr.Error()
			}
			accounts = append(accounts, Account{
				ID: sourceAccountID(root, profile, identity.Gaia), GaiaID: identity.Gaia,
				ChromeRoot: root, Profile: profile, DisplayName: info.Name, Email: identity.Email, Locale: locale,
				Importable: tokenErr == nil && len(binding) != 0, TokenFormat: format, Reason: reason,
			})
		}
	}
	return accounts, nil
}

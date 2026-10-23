//go:build windows

package chromeauth

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"
)

func defaultChromeRoot() (string, error) {
	localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if localAppData == "" {
		return "", fmt.Errorf("环境变量 LOCALAPPDATA 为空")
	}
	return filepath.Join(localAppData, "Google", "Chrome", "User Data"), nil
}

func ensurePlatformImport() error {
	return nil
}

func discoverPlatform(chromeRoot string) ([]Account, error) {
	return discoverPlatformProfiles(chromeRoot, "")
}

// profileIdentity 为 Preferences.account_info 中的一个 Google 账号
type profileIdentity struct {
	Gaia  string `json:"gaia"`
	Email string `json:"email"`
}

// profileIdentities 读取 Profile 内全部 Google 账号的 Gaia ID 与邮箱
func profileIdentities(chromeRoot string, profile string) ([]profileIdentity, error) {
	p, err := checkedChromePath(chromeRoot, profile, "Preferences")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取 %s Preferences: %w", profile, err)
	}
	var preferences struct {
		Accounts []profileIdentity `json:"account_info"`
	}
	if err := json.Unmarshal(data, &preferences); err != nil {
		return nil, fmt.Errorf("解析 %s Preferences: %w", profile, err)
	}
	return preferences.Accounts, nil
}

func profileLocale(chromeRoot string, profile string) string {
	p, err := checkedChromePath(chromeRoot, profile, "Preferences")
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var preferences struct {
		Intl struct {
			AcceptLanguages   string `json:"accept_languages"`
			SelectedLanguages string `json:"selected_languages"`
		} `json:"intl"`
	}
	if json.Unmarshal(data, &preferences) != nil {
		return ""
	}
	for _, value := range []string{preferences.Intl.AcceptLanguages, preferences.Intl.SelectedLanguages} {
		if locale := strings.TrimSpace(strings.Split(value, ",")[0]); locale != "" {
			return locale
		}
	}
	return ""
}

func readTokenService(chromeRoot string, profile string, gaiaID string) (string, []byte, []byte, error) {
	if strings.TrimSpace(gaiaID) == "" {
		return "", nil, nil, fmt.Errorf("缺少精确 Gaia 身份")
	}
	databasePath, err := checkedChromePath(chromeRoot, profile, "Web Data")
	if err != nil {
		return "", nil, nil, err
	}
	if err := ensureNoPendingWAL(databasePath); err != nil {
		return "", nil, nil, err
	}
	database, err := sql.Open("sqlite", tokenDatabaseURI(databasePath))
	if err != nil {
		return "", nil, nil, fmt.Errorf("打开 Chrome Web Data 失败")
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	hasMTLS, err := tokenServiceHasMTLS(database)
	if err != nil {
		return "", nil, nil, err
	}
	query := "SELECT service, encrypted_token, binding_key, 0 FROM token_service WHERE service = ?"
	if hasMTLS {
		query = "SELECT service, encrypted_token, binding_key, COALESCE(mtls_token_binding,0) FROM token_service WHERE service = ?"
	}
	rows, err := database.Query(query, "AccountId-"+gaiaID)
	if err != nil {
		return "", nil, nil, fmt.Errorf("读取 Chrome token_service 失败")
	}
	defer rows.Close()
	if !rows.Next() {
		return "", nil, nil, fmt.Errorf("精确身份没有 token_service 记录")
	}
	var service string
	var encryptedToken, bindingKey []byte
	var mtls int64
	if err := rows.Scan(&service, &encryptedToken, &bindingKey, &mtls); err != nil {
		return "", nil, nil, fmt.Errorf("解析 Chrome token_service 失败")
	}
	if rows.Next() {
		return "", nil, nil, fmt.Errorf("精确身份含重复 token_service 记录")
	}
	if err := rows.Err(); err != nil {
		return "", nil, nil, fmt.Errorf("读取 Chrome token_service 失败")
	}
	if mtls != 0 {
		return "", nil, nil, fmt.Errorf("不支持 Chrome mTLS token binding")
	}
	if service != "AccountId-"+gaiaID || len(encryptedToken) == 0 || len(bindingKey) == 0 {
		return "", nil, nil, fmt.Errorf("Chrome token_service 身份或设备绑定材料不完整")
	}
	if err := ensureNoPendingWAL(databasePath); err != nil {
		return "", nil, nil, err
	}
	return gaiaID, encryptedToken, bindingKey, nil
}

func naturalLess(left string, right string) bool {
	leftParts := splitNatural(left)
	rightParts := splitNatural(right)
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		leftNumber, leftErr := strconv.Atoi(leftParts[index])
		rightNumber, rightErr := strconv.Atoi(rightParts[index])
		if leftErr == nil && rightErr == nil {
			if leftNumber != rightNumber {
				return leftNumber < rightNumber
			}
			continue
		}
		comparison := strings.Compare(strings.ToLower(leftParts[index]), strings.ToLower(rightParts[index]))
		if comparison != 0 {
			return comparison < 0
		}
	}
	return len(leftParts) < len(rightParts)
}

func splitNatural(value string) []string {
	if value == "" {
		return nil
	}
	parts := make([]string, 0)
	start := 0
	lastDigit := unicode.IsDigit(rune(value[0]))
	for index, character := range value {
		isDigit := unicode.IsDigit(character)
		if index != 0 && isDigit != lastDigit {
			parts = append(parts, value[start:index])
			start = index
		}
		lastDigit = isDigit
	}
	return append(parts, value[start:])
}

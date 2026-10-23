package chromeauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// readDPAPICiphertext uses only the selected root's native encrypted_key.
func readDPAPICiphertext(root string) ([]byte, error) {
	p, err := checkedChromePath(root, "", "Local State")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取 Chrome Local State 失败")
	}
	var state struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if json.Unmarshal(data, &state) != nil {
		return nil, fmt.Errorf("解析 Chrome Local State 失败")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(state.OSCrypt.EncryptedKey))
	if err != nil || len(raw) <= 5 || string(raw[:5]) != "DPAPI" {
		return nil, fmt.Errorf("Chrome 原生密钥缺少有效 DPAPI 封装")
	}
	return append([]byte(nil), raw[5:]...), nil
}

func retrieveV10Key(root string) ([]byte, error) {
	encrypted, err := readDPAPICiphertext(root)
	if err != nil {
		return nil, err
	}
	key, err := unprotectChromeDPAPI(encrypted)
	if err != nil {
		return nil, fmt.Errorf("当前 Windows 用户无法解包 Chrome v10 密钥: %w", err)
	}
	if len(key) != 32 {
		clear(key)
		return nil, fmt.Errorf("Chrome v10 主密钥长度异常")
	}
	return key, nil
}

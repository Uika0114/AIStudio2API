//go:build !windows

package chromeauth

import "fmt"

func unprotectChromeDPAPI([]byte) ([]byte, error) {
	return nil, fmt.Errorf("Chrome OAuth 导入仅支持 Windows")
}
func rejectChromeReparsePath(string) error { return nil }

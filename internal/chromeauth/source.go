package chromeauth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// ErrSourceBindingRequired forbids guessing a browser source for legacy state.
var ErrSourceBindingRequired = errors.New("Chrome 来源不完整，需要显式绑定或重绑来源")

// NormalizeChromeRoot resolves a user-selected root, never a Profile directory.
// Empty is the explicit default-import contract, not an authentication fallback.
func NormalizeChromeRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		var err error
		root, err = DefaultChromeRoot()
		if err != nil {
			return "", err
		}
	}
	if strings.ContainsAny(root, "\x00\r\n") {
		return "", fmt.Errorf("Chrome 根目录包含非法字符")
	}
	if strings.HasPrefix(root, `\\`) || strings.HasPrefix(root, "//") {
		return "", fmt.Errorf("不支持网络或设备 Chrome 根目录")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("Chrome 根目录必须是本机绝对路径")
	}
	if runtime.GOOS == "windows" && len(filepath.VolumeName(root)) != 2 {
		return "", fmt.Errorf("Chrome 根目录必须使用本机盘符")
	}
	for _, part := range strings.FieldsFunc(root, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "." || part == ".." || strings.TrimRight(part, " .") != part || strings.Contains(part, "~") {
			return "", fmt.Errorf("Chrome 根目录不能包含路径别名或相对段")
		}
	}
	return filepath.Clean(root), nil
}

func sameChromeRoot(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func validateProfileName(profile string) error {
	if profile == "" || profile == "." || profile == ".." || strings.TrimSpace(profile) != profile || strings.TrimRight(profile, " .") != profile || strings.ContainsAny(profile, "/\\:\x00\r\n") || strings.Contains(profile, "~") {
		return fmt.Errorf("Chrome Profile 名称无效")
	}
	return nil
}

// Account IDs include root identity, so a selection from another root is stale.
func sourceAccountID(root, profile, gaia string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.ToSlash(filepath.Clean(root)))))
	return "chrome-" + hex.EncodeToString(sum[:]) + "/" + profile + "/" + gaia
}

// ValidateSource checks identity without probing the filesystem or default root.
func ValidateSource(source aistudio.AuthSource, material *aistudio.ChromeOAuthMaterial) (aistudio.AuthSource, error) {
	if source.Browser != "chrome" || strings.TrimSpace(source.ChromeRoot) == "" || validateProfileName(source.Profile) != nil || !strings.Contains(source.Email, "@") {
		return aistudio.AuthSource{}, ErrSourceBindingRequired
	}
	root, err := NormalizeChromeRoot(source.ChromeRoot)
	if err != nil {
		return aistudio.AuthSource{}, fmt.Errorf("%w: %v", ErrSourceBindingRequired, err)
	}
	if material != nil {
		if source.GaiaID != "" && source.GaiaID != material.GaiaID {
			return aistudio.AuthSource{}, fmt.Errorf("Chrome 来源与保存材料的 Gaia 身份不匹配")
		}
		if source.GaiaID == "" {
			source.GaiaID = material.GaiaID
		}
	}
	if strings.TrimSpace(source.GaiaID) == "" {
		return aistudio.AuthSource{}, ErrSourceBindingRequired
	}
	source.ChromeRoot = root
	source.Email = strings.ToLower(strings.TrimSpace(source.Email))
	return source, nil
}

// SameSource compares full identity, not email or a preferred Profile alone.
func SameSource(left, right aistudio.AuthSource) bool {
	a, err := ValidateSource(left, nil)
	if err != nil {
		return false
	}
	b, err := ValidateSource(right, nil)
	if err != nil {
		return false
	}
	return sameChromeRoot(a.ChromeRoot, b.ChromeRoot) && strings.EqualFold(a.Profile, b.Profile) && a.GaiaID == b.GaiaID && strings.EqualFold(a.Email, b.Email)
}

// checkedChromePath rejects links/reparse points before native source reads.
func checkedChromePath(root, profile, name string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrSourceBindingRequired
	}
	canonical, err := NormalizeChromeRoot(root)
	if err != nil {
		return "", err
	}
	if profile != "" {
		if err := validateProfileName(profile); err != nil {
			return "", err
		}
	}
	p := filepath.Join(canonical, profile, name)
	for cursor := p; ; cursor = filepath.Dir(cursor) {
		info, err := os.Lstat(cursor)
		if err != nil {
			return "", fmt.Errorf("Chrome 来源路径不可用: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("不支持 Chrome 来源符号链接或目录别名")
		}
		if err := rejectChromeReparsePath(cursor); err != nil {
			return "", err
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
	}
	return p, nil
}

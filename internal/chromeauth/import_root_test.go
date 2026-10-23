package chromeauth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// Deterministic nonce and invented token are fixtures, never browser material.
func fixtureEncryptedToken(t *testing.T, format string, key []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{7}, gcm.NonceSize())
	out := append([]byte(format), nonce...)
	return gcm.Seal(out, nonce, []byte("1//0"+strings.Repeat("x", 99)), nil)
}

func fixtureAccount(root, profile, gaia, email string) Account {
	return Account{ID: sourceAccountID(root, profile, gaia), ChromeRoot: root, Profile: profile, GaiaID: gaia, Email: email, Importable: true}
}

func TestImportFormatsKeepRootAndSource(t *testing.T) {
	for _, formats := range [][]string{{"v10"}, {"v20"}, {"v10", "v20", "v10"}} {
		t.Run(strings.Join(formats, "-"), func(t *testing.T) {
			root := t.TempDir()
			key := bytes.Repeat([]byte{42}, 32)
			accounts := make([]Account, len(formats))
			encrypted := map[string][]byte{}
			for i, format := range formats {
				gaia := strings.Repeat("1", i+1)
				accounts[i] = fixtureAccount(root, "Default", gaia, gaia+"@example.com")
				encrypted[gaia] = fixtureEncryptedToken(t, format, key)
			}
			keyCalls := map[string]int{}
			cookieCalls := 0
			backend := importBackend{
				discover: func(r, p string) ([]Account, error) {
					if r != root || p != "" {
						t.Fatal("wrong discovery source")
					}
					return accounts, nil
				},
				readToken: func(r, p, g string) (string, []byte, []byte, error) {
					if r != root || p != "Default" {
						t.Fatal("wrong token source")
					}
					return g, encrypted[g], []byte("fixture-binding"), nil
				},
				key: func(r, f string) ([]byte, error) {
					if r != root {
						t.Fatal("wrong key root")
					}
					keyCalls[f]++
					return append([]byte(nil), key...), nil
				},
				cookies: func(_ context.Context, g, token string, b []byte, proxy string) ([]multiloginCookie, error) {
					cookieCalls++
					if token != "1//0"+strings.Repeat("x", 99) || string(b) != "fixture-binding" {
						t.Fatal("wrong fixture exchange")
					}
					return []multiloginCookie{{Name: "SAPISID", Value: "fixture-cookie", Domain: ".google.com", Path: "/"}}, nil
				},
			}
			ids := make([]string, len(accounts))
			for i, a := range accounts {
				ids[i] = a.ID
			}
			results, err := importWithBackend(context.Background(), ImportOptions{ChromeRoot: root, AccountIDs: ids}, backend)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != len(formats) || cookieCalls != len(formats) {
				t.Fatal("wrong import count")
			}
			for _, f := range formats {
				if keyCalls[f] != 1 {
					t.Fatal("key must be lazy and cached per format")
				}
			}
			if len(formats) == 1 && formats[0] == "v10" && keyCalls["v20"] != 0 {
				t.Fatal("v10 called v20 helper")
			}
			for i, result := range results {
				raw, err := json.Marshal(result.State)
				if err != nil {
					t.Fatal(err)
				}
				var reloaded aistudio.StorageState
				if err := json.Unmarshal(raw, &reloaded); err != nil {
					t.Fatal(err)
				}
				ext, ok, err := reloaded.AuthExtension()
				if err != nil || !ok {
					t.Fatal("lost extension")
				}
				if ext.Source.ChromeRoot != root || ext.Source.Profile != "Default" || ext.Source.GaiaID != accounts[i].GaiaID || ext.OAuth.GaiaID != accounts[i].GaiaID {
					t.Fatal("lost precise source")
				}
			}
		})
	}
}

func TestImportRejectsCrossRootBeforeKeyOrHTTP(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	account := fixtureAccount(b, "Default", "111", "fixture@example.com")
	calls := 0
	backend := importBackend{
		discover: func(string, string) ([]Account, error) { return []Account{account}, nil },
		readToken: func(string, string, string) (string, []byte, []byte, error) {
			calls++
			return "", nil, nil, errors.New("unexpected")
		},
		key: func(string, string) ([]byte, error) { calls++; return nil, errors.New("unexpected") },
		cookies: func(context.Context, string, string, []byte, string) ([]multiloginCookie, error) {
			calls++
			return nil, errors.New("unexpected")
		},
	}
	_, err := importWithBackend(context.Background(), ImportOptions{ChromeRoot: b, AccountIDs: []string{sourceAccountID(a, "Default", "111")}}, backend)
	if err == nil || calls != 0 {
		t.Fatal("cross-root selection reached key or HTTP")
	}
}

func TestExactImportUsesOnlyBoundProfile(t *testing.T) {
	root := t.TempDir()
	key := bytes.Repeat([]byte{42}, 32)
	account := fixtureAccount(root, "Profile 2", "111", "fixture@example.com")
	source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: root, Profile: account.Profile, GaiaID: account.GaiaID, Email: account.Email}
	backend := importBackend{
		discover: func(r, p string) ([]Account, error) {
			if r != root || p != account.Profile {
				t.Fatal("read another Profile")
			}
			return []Account{account}, nil
		},
		readToken: func(r, p, g string) (string, []byte, []byte, error) {
			return g, fixtureEncryptedToken(t, "v10", key), []byte("fixture-binding"), nil
		},
		key: func(string, string) ([]byte, error) { return append([]byte(nil), key...), nil },
		cookies: func(context.Context, string, string, []byte, string) ([]multiloginCookie, error) {
			return []multiloginCookie{}, nil
		},
	}
	if _, err := importWithBackend(context.Background(), ImportOptions{ExactSource: &source}, backend); err != nil {
		t.Fatal(err)
	}
	source.ChromeRoot = ""
	backend.discover = func(string, string) ([]Account, error) { t.Fatal("legacy source probed default root"); return nil, nil }
	if _, err := importWithBackend(context.Background(), ImportOptions{ExactSource: &source}, backend); !errors.Is(err, ErrSourceBindingRequired) {
		t.Fatal("missing root must require explicit binding")
	}
}

func TestSourceValidationAndTokenFailures(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"relative", "../escape", root + "/../escape", root + "/trailing.", "\\\\server\\share"} {
		if _, err := NormalizeChromeRoot(p); err == nil {
			t.Fatalf("accepted unsafe root %q", p)
		}
	}
	for _, p := range []string{"../Default", "Default/child", "Default.", "Default ", "C:Default"} {
		if validateProfileName(p) == nil {
			t.Fatal("accepted unsafe Profile")
		}
	}
	source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: root, Profile: "Default", GaiaID: "111", Email: "fixture@example.com"}
	if _, err := ValidateSource(source, &aistudio.ChromeOAuthMaterial{GaiaID: "222"}); err == nil {
		t.Fatal("accepted wrong material identity")
	}
	other := source
	other.ChromeRoot = filepath.Join(root, "other")
	if SameSource(source, other) {
		t.Fatal("equal email must not equate roots")
	}
	key := bytes.Repeat([]byte{42}, 32)
	valid := fixtureEncryptedToken(t, "v10", key)
	unknown := append([]byte(nil), valid...)
	copy(unknown[:3], "v99")
	corrupt := append([]byte(nil), valid...)
	corrupt[len(corrupt)-1] ^= 1
	for _, encrypted := range [][]byte{[]byte("v10"), unknown, corrupt} {
		if _, err := decryptChromeToken(key, encrypted); err == nil {
			t.Fatal("accepted unsupported or damaged token")
		}
	}
	if _, err := decryptV20Token(key, valid); err == nil {
		t.Fatal("legacy v20 decoder accepted v10")
	}
	if _, err := decryptChromeToken(key[:16], valid); err == nil {
		t.Fatal("accepted short key")
	}
}

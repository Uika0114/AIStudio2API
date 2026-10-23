//go:build windows

package chromeauth

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"golang.org/x/sys/windows"
)

func fixtureNativeChromeRoot(t *testing.T, format string, mtls int) (string, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Default"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Local State"), []byte(`{"profile":{"info_cache":{"Default":{"name":"Fixture","user_name":"fixture@example.com"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Default", "Preferences"), []byte(`{"account_info":[{"gaia":"111","email":"fixture@example.com"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Default", "Web Data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE token_service(service TEXT PRIMARY KEY,encrypted_token BLOB,binding_key BLOB,mtls_token_binding INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO token_service VALUES(?,?,?,?)", "AccountId-111", fixtureEncryptedToken(t, format, bytes.Repeat([]byte{42}, 32)), []byte("fixture-binding"), mtls); err != nil {
		t.Fatal(err)
	}
	return root, db
}

func TestNativeDiscoveryKeepsRootAndExactProfile(t *testing.T) {
	a, dbA := fixtureNativeChromeRoot(t, "v20", 0)
	dbA.Close()
	b, dbB := fixtureNativeChromeRoot(t, "v10", 0)
	dbB.Close()
	t.Setenv("LOCALAPPDATA", t.TempDir()) // No real default browser source is available.
	accountsA, err := Discover(a)
	if err != nil {
		t.Fatal(err)
	}
	accountsB, err := Discover(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(accountsA) != 1 || len(accountsB) != 1 || !accountsB[0].Importable || accountsB[0].TokenFormat != "v10" || accountsA[0].ID == accountsB[0].ID {
		t.Fatal("root scope or v10 eligibility lost")
	}
	// An unrelated invalid entry would fail a whole-root scan, not the bound lookup.
	if err := os.WriteFile(filepath.Join(b, "Local State"), []byte(`{"profile":{"info_cache":{"Default":{"name":"Fixture"},"../escape":{"name":"Never read"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: b, Profile: "Default", GaiaID: "111", Email: "fixture@example.com"}
	if _, err := SourceAccount(source, nil); err != nil {
		t.Fatal("exact lookup touched an unrelated Profile")
	}
	if _, err := Discover(b); err == nil {
		t.Fatal("discovery accepted a Profile traversal")
	}
	source.Profile = "Profile 2"
	if _, err := SourceAccount(source, nil); err == nil {
		t.Fatal("missing Profile fell back to Default")
	}
}

func TestNativeDatabaseRejectsMTLSAndPendingWAL(t *testing.T) {
	root, db := fixtureNativeChromeRoot(t, "v10", 1)
	db.Close()
	if _, _, _, err := readTokenService(root, "Default", "111"); err == nil || !strings.Contains(err.Error(), "mTLS") {
		t.Fatal("accepted unsupported mTLS binding")
	}
	accounts, err := Discover(root)
	if err != nil || accounts[0].Importable {
		t.Fatal("mTLS account advertised as importable")
	}
	root, db = fixtureNativeChromeRoot(t, "v10", 0)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE token_service SET binding_key=?", []byte("fixture-wal-binding")); err != nil {
		t.Fatal(err)
	}
	wal, walErr := os.Stat(filepath.Join(root, "Default", "Web Data-wal"))
	if walErr != nil || wal.Size() == 0 {
		t.Fatal("fixture did not create a committed WAL")
	}
	if _, _, _, err := readTokenService(root, "Default", "111"); err == nil || !strings.Contains(err.Error(), "WAL") {
		t.Fatalf("expected WAL rejection, got: %v", err)
	}
	db.Close()
	if _, _, _, err := readTokenService(root, "Default", "111"); err != nil {
		t.Fatal("closed fixture could not be read after checkpoint")
	}
}

func TestV10NativeDPAPIUsesOnlySyntheticKey(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	in := windows.DataBlob{Size: uint32(len(key)), Data: &key[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		t.Fatal("native synthetic DPAPI protection failed")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	protected := append([]byte("DPAPI"), unsafe.Slice(out.Data, int(out.Size))...)
	root := t.TempDir()
	raw, err := json.Marshal(map[string]any{"os_crypt": map[string]string{"encrypted_key": base64.StdEncoding.EncodeToString(protected)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Local State"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := retrieveV10Key(root)
	if err != nil || !bytes.Equal(key, got) {
		t.Fatal("native synthetic DPAPI round trip failed")
	}
	clear(got)
	if err := os.WriteFile(filepath.Join(root, "Local State"), []byte(`{"os_crypt":{"encrypted_key":"not-a-dpapi-key"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := retrieveV10Key(root); err == nil {
		t.Fatal("invalid key wrapper was accepted")
	}
}

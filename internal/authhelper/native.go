package authhelper

import (
	"context"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"os/user"
	"runtime"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

// WindowsIdentity refuses SYSTEM and records the original interactive-user identity.
func WindowsIdentity() (string, error) {
	if runtime.GOOS != "windows" {
		return "", failure("UNSUPPORTED_KEY")
	}
	current, err := user.Current()
	if err != nil || current.Uid == "" || current.Uid == "S-1-5-18" || current.Uid == "S-1-5-19" || current.Uid == "S-1-5-20" {
		return "", failure("UNSUPPORTED_KEY")
	}
	return current.Uid, nil
}

// NativeRenew is called only by the Windows helper after a fixed mapping is checked.
func NativeRenew(ctx context.Context, state aistudio.StorageState, m Mapping, allowSource bool) (aistudio.StorageState, error) {
	if runtime.GOOS != "windows" {
		return state, failure("UNSUPPORTED_KEY")
	}
	sid, identityErr := WindowsIdentity()
	meta, stateErr := state.Session()
	if identityErr != nil || stateErr != nil || meta == nil || meta.Authority == nil || meta.Authority.WindowsUserSID != sid {
		return state, failure("UNSUPPORTED_KEY")
	}
	return nativeRenewWith(ctx, state, m, allowSource, chromeauth.Refresh, chromeauth.Import)
}

func nativeRenewWith(ctx context.Context, state aistudio.StorageState, m Mapping, allowSource bool, refresh func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error), importer exactImportFunc) (aistudio.StorageState, error) {
	ext, exists, err := state.AuthExtension()
	if err != nil || !exists || ext.OAuth == nil || ext.OAuth.GaiaID != m.GaiaID || ext.Source.Email != m.Email {
		return state, failure("IDENTITY_MISMATCH")
	}
	proxy, err := nativeProxy(m)
	if err != nil {
		return state, err
	}
	cookies, err := refresh(ctx, *ext.OAuth, proxy)
	if errors.Is(err, chromeauth.ErrCredentialsRejected) {
		if !allowSource {
			return state, failure("MATERIAL_REJECTED")
		}
		source, sourceErr := chromeauth.ValidateSource(ext.Source, ext.OAuth)
		if sourceErr != nil {
			return state, failure("SOURCE_REBIND_REQUIRED")
		}
		imported, importErr := importer(ctx, chromeauth.ImportOptions{ChromeRoot: source.ChromeRoot, ExactSource: &source, Proxy: proxy})
		if importErr != nil {
			return state, failure("INDETERMINATE")
		}
		if len(imported) != 1 {
			return state, failure("IDENTITY_MISMATCH")
		}
		next, ok, nextErr := imported[0].State.AuthExtension()
		if nextErr != nil || !ok || next.OAuth == nil || !chromeauth.SameSource(source, next.Source) || next.OAuth.GaiaID != m.GaiaID {
			return state, failure("IDENTITY_MISMATCH")
		}
		if state.SetAuthExtension(next) != nil {
			return state, failure("SOURCE_REBIND_REQUIRED")
		}
		cookies, err = imported[0].State.Cookies, nil
	}
	if err != nil {
		return state, failure("INDETERMINATE")
	}
	state.Cookies = cookies
	return state, nil
}

func nativeProxy(m Mapping) (string, error) {
	if m.DirectAuth && m.ProxyFile == "" {
		return "", nil
	}
	if m.DirectAuth || m.ProxyFile == "" {
		return "", failure("SOURCE_REBIND_REQUIRED")
	}
	f, err := os.Open(m.ProxyFile)
	if err != nil {
		return "", failure("SOURCE_REBIND_REQUIRED")
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(value) > 16384 {
		return "", failure("SOURCE_REBIND_REQUIRED")
	}
	proxy := strings.TrimSpace(string(value))
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(proxy, "\r\n") {
		return "", failure("SOURCE_REBIND_REQUIRED")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return proxy, nil
	}
	return "", failure("SOURCE_REBIND_REQUIRED")
}

// RegisterAuthority consumes one explicitly named native state. It never discovers
// profiles/accounts, changes account config, or enables a mapping by default.
func RegisterAuthority(ctx context.Context, m Mapping) error {
	sid, err := WindowsIdentity()
	if err != nil {
		return err
	}
	return registerAuthority(ctx, m, sid)
}

func registerAuthority(ctx context.Context, m Mapping, sid string) error {
	if _, err := NewBroker(ctx, []Mapping{m}, NativeRenew); err != nil {
		return err
	}
	release, err := aistudio.AcquireSessionAccountLease(ctx, m.StateFile)
	if err != nil {
		return err
	}
	defer release()
	state, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	previous, err := state.Session()
	if err != nil || previous != nil {
		return failure("OPERATION_CONFLICT")
	}
	ext, exists, err := state.AuthExtension()
	if err != nil || !exists || ext.OAuth == nil || ext.OAuth.GaiaID != m.GaiaID || ext.Source.Email != m.Email {
		return failure("IDENTITY_MISMATCH")
	}
	if _, err := chromeauth.ValidateSource(ext.Source, ext.OAuth); err != nil {
		return failure("SOURCE_REBIND_REQUIRED")
	}
	if err := ValidateCookies(state.Cookies, m.CookieDomains); err != nil {
		return err
	}
	meta := &aistudio.SessionState{Version: Protocol, Authority: &aistudio.SessionAuthority{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1, WindowsUserSID: sid}}
	if retired, retiredErr := state.RetiredAuthority(); retiredErr != nil {
		return failure("STATE_UNAVAILABLE")
	} else if retired != nil {
		if retired.GaiaID != m.GaiaID || retired.Email != m.Email || retired.WindowsUserSID != sid || retired.Epoch == math.MaxUint64 || retired.MaterialVersion == math.MaxUint64 || retired.CookieRevision == math.MaxUint64 {
			return failure("IDENTITY_MISMATCH")
		}
		meta.Authority.Epoch = retired.Epoch + 1
		meta.Authority.CookieRevision = retired.CookieRevision + 1
		meta.Authority.MaterialVersion = retired.MaterialVersion + 1
		meta.Authority.History = append(meta.Authority.History, retired.History...)
		if retired.Operation != nil {
			meta.Authority.History = append(meta.Authority.History, *retired.Operation)
		}
	}
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, m.StateFile, &state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return nil
}

func SetAuthorityPolicy(ctx context.Context, m Mapping, expectedEpoch uint64, enabled, allowSource bool) error {
	release, err := aistudio.AcquireSessionAccountLease(ctx, m.StateFile)
	if err != nil {
		return err
	}
	defer release()
	state, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Authority == nil {
		return failure("MAPPING_DISABLED")
	}
	a := meta.Authority
	if a.Epoch != expectedEpoch || a.Handle != m.Handle || a.Consumer != m.Consumer || a.GaiaID != m.GaiaID || a.Email != m.Email {
		return failure("VERSION_CONFLICT")
	}
	if enabled && a.Operation != nil && (a.Operation.Stage == "pending" || a.Operation.Stage == "indeterminate") {
		return failure("INDETERMINATE")
	}
	if a.Enabled == enabled && a.AllowSourceRecovery == allowSource {
		return nil
	}
	if a.Epoch == ^uint64(0) {
		return failure("VERSION_CONFLICT")
	}
	a.Epoch++
	a.Enabled, a.AllowSourceRecovery = enabled, allowSource
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, m.StateFile, &state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	return nil
}

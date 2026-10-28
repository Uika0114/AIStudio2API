package authhelper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

type exactImportFunc func(context.Context, chromeauth.ImportOptions) ([]chromeauth.ImportResult, error)

// RebindAuthoritySource is a native, explicitly confirmed account operation. It is
// never exposed over RPC, never called as a network-error fallback, and never
// overwrites a live mapping. The Gaia/email identity remains fixed.
func RebindAuthoritySource(ctx context.Context, m Mapping, epoch uint64, source aistudio.AuthSource, acknowledgePrior bool) error {
	sid, err := WindowsIdentity()
	if err != nil {
		return err
	}
	return rebindAuthoritySource(ctx, m, epoch, source, acknowledgePrior, sid, chromeauth.Import)
}

func rebindAuthoritySource(ctx context.Context, m Mapping, epoch uint64, source aistudio.AuthSource, acknowledgePrior bool, sid string, importer exactImportFunc) error {
	source, err := chromeauth.ValidateSource(source, nil)
	if err != nil || source.GaiaID != m.GaiaID || source.Email != m.Email {
		return failure("IDENTITY_MISMATCH")
	}
	proxy, err := nativeProxy(m)
	if err != nil {
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
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Authority == nil {
		return failure("MAPPING_DISABLED")
	}
	a := meta.Authority
	if a.Enabled || a.Epoch != epoch || a.Handle != m.Handle || a.Consumer != m.Consumer || a.GaiaID != m.GaiaID || a.Email != m.Email || a.WindowsUserSID != sid {
		return failure("IDENTITY_MISMATCH")
	}
	if a.Epoch == ^uint64(0) || a.CookieRevision == ^uint64(0) || a.MaterialVersion == ^uint64(0) {
		return failure("VERSION_CONFLICT")
	}
	if a.Operation != nil {
		if (a.Operation.Stage == "pending" || a.Operation.Stage == "indeterminate" || a.Operation.Stage == "ready") && !acknowledgePrior {
			return failure("ACK_REQUIRED")
		}
		a.History = append(a.History, *a.Operation)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	deadline := time.Now().Add(90 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	a.Operation = &aistudio.SessionOperation{ID: hex.EncodeToString(id[:]), Reason: "reauth", Epoch: a.Epoch, BaseCookie: a.CookieRevision, BaseMaterial: a.MaterialVersion, Deadline: deadline.Unix(), Stage: "pending"}
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, m.StateFile, &state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	meta, _ = state.Session()
	a = meta.Authority
	ctx, httpCounts := countAuthentication(ctx)
	results, importErr := importer(ctx, chromeauth.ImportOptions{ChromeRoot: source.ChromeRoot, ExactSource: &source, Proxy: proxy})
	a.Operation.HTTP = httpCounts.snapshot()
	if importErr == nil && len(results) != 1 {
		importErr = failure("IDENTITY_MISMATCH")
	}
	if importErr == nil {
		ext, exists, e := results[0].State.AuthExtension()
		if e != nil || !exists || ext.OAuth == nil || ext.OAuth.GaiaID != m.GaiaID || !chromeauth.SameSource(source, ext.Source) {
			importErr = failure("IDENTITY_MISMATCH")
		} else if _, e = projectCookies(results[0].State.Cookies, m.CookieDomains); e != nil {
			importErr = e
		} else {
			state.Cookies = results[0].State.Cookies
			if state.SetAuthExtension(ext) != nil {
				importErr = failure("IDENTITY_MISMATCH")
			}
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if importErr != nil {
		original, err := aistudio.LoadStorageState(m.StateFile)
		if err != nil {
			return failure("INDETERMINATE")
		}
		a.Operation.Stage, a.Operation.Failure = "indeterminate", "INDETERMINATE"
		if original.SetSession(meta) != nil || aistudio.WriteManagedSessionState(save, m.StateFile, &original) != nil {
			return failure("INDETERMINATE")
		}
		return failure("INDETERMINATE")
	}
	a.CookieRevision++
	a.MaterialVersion++
	a.Operation.Stage = "acked"
	a.Operation.ResultCookie, a.Operation.ResultMaterial = a.CookieRevision, a.MaterialVersion
	a.Epoch++ // Still disabled. The Linux account must explicitly enroll this epoch.
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(save, m.StateFile, &state) != nil {
		return failure("INDETERMINATE")
	}
	return nil
}

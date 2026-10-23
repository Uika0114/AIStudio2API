package authhelper

import (
	"context"
	"path/filepath"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// ReenrollNative is an explicit local control operation after draining/disabling
// this Linux account. It preserves cookies/config and retains retired metadata.
// The new epoch is unusable for generation until bootstrap commits successfully.
func ReenrollNative(ctx context.Context, client *Client, path string, binding aistudio.RemoteSession, acknowledgePrior bool) error {
	if !filepath.IsAbs(path) {
		return failure("INVALID_REQUEST")
	}
	release, err := aistudio.AcquireSessionAccountLease(ctx, path)
	if err != nil {
		return err
	}
	defer release()
	account, err := aistudio.ReadAccountDirectory(filepath.Dir(path))
	if err != nil || filepath.Clean(account.StoragePath) != filepath.Clean(path) || account.Config.Enabled || account.ID != binding.Email {
		return failure("BUSY")
	}
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	meta, err := state.Session()
	if err != nil || meta == nil || meta.Remote == nil {
		return failure("IDENTITY_MISMATCH")
	}
	old := meta.Remote
	if binding.Epoch <= old.Epoch || binding.GaiaID != old.GaiaID || binding.Email != old.Email || binding.CookieRevision < old.CookieRevision || binding.MaterialVersion < old.MaterialVersion || binding.Consumer != client.Consumer() {
		return failure("VERSION_CONFLICT")
	}
	if (old.Pending != nil || old.Ack != nil) && !acknowledgePrior {
		return failure("ACK_REQUIRED")
	}
	binding.Retired = append(append([]aistudio.RetiredRemoteBinding(nil), old.Retired...), aistudio.RetiredRemoteBinding{Handle: old.Handle, Consumer: old.Consumer, Epoch: old.Epoch, CookieRevision: old.CookieRevision, MaterialVersion: old.MaterialVersion, Pending: old.Pending, Ack: old.Ack})
	binding.EnrollmentRequired = true
	binding.Pending, binding.Ack = nil, nil
	meta.Remote = &binding
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, path, &state) != nil {
		return failure("STATE_UNAVAILABLE")
	}
	if err := client.PrepareRecovery(ctx, &state, path, "bootstrap", nil); err != nil {
		return err
	}
	return commitNativeProjection(ctx, client, &state, path)
}

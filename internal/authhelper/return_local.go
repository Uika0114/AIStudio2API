package authhelper

import (
	"context"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func ReturnLocalAuthority(ctx context.Context, m Mapping, epoch uint64, oldConsumerStopped bool) error {
	sid, err := WindowsIdentity()
	if err != nil {
		return err
	}
	return returnLocalAuthority(ctx, m, epoch, oldConsumerStopped, sid)
}
func returnLocalAuthority(ctx context.Context, m Mapping, epoch uint64, oldConsumerStopped bool, sid string) error {
	if !oldConsumerStopped {
		return failure("INVALID_REQUEST")
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
		return failure("IDENTITY_MISMATCH")
	}
	a := meta.Authority
	if a.Enabled || a.Epoch != epoch || a.Handle != m.Handle || a.Consumer != m.Consumer || a.GaiaID != m.GaiaID || a.Email != m.Email || a.WindowsUserSID != sid {
		return failure("IDENTITY_MISMATCH")
	}
	return aistudio.ReturnSessionAuthorityToLocal(ctx, m.StateFile, &state)
}

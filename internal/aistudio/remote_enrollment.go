package aistudio

// RetiredRemoteBinding preserves pending/ack metadata when a native operator
// explicitly fences an old epoch. There are no cookies or credential snapshots.
type RetiredRemoteBinding struct {
	Handle          string            `json:"handle"`
	Consumer        string            `json:"consumer"`
	Epoch           uint64            `json:"epoch"`
	CookieRevision  uint64            `json:"cookie_revision"`
	MaterialVersion uint64            `json:"material_version"`
	Pending         *SessionOperation `json:"pending,omitempty"`
	Ack             *SessionAck       `json:"ack,omitempty"`
}

// ReadAccountDirectory reads exactly one selected native account, never discovers.
func ReadAccountDirectory(directory string) (*Account, error) { return loadAccount(directory) }

func sessionAcquisitionFence(path string) error {
	state, err := LoadStorageState(path)
	if err != nil {
		return err
	}
	meta, err := state.Session()
	if err != nil {
		return err
	}
	if meta == nil {
		return nil
	}
	if meta.Authority != nil {
		return SessionError("REMOTE_OWNED")
	}
	if meta.Remote.EnrollmentRequired {
		return SessionError("BOOTSTRAP_REQUIRED")
	}
	return nil
}

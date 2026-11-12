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
	return sessionAcquisitionFenceFor(path, leaseGeneration)
}

func sessionAcquisitionFenceFor(path string, purpose accountLeasePurpose) error {
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
	if purpose == leaseManagement || purpose == leaseRemoteMaintenance {
		return nil
	}
	if meta.Remote.EnrollmentRequired {
		return SessionError("BOOTSTRAP_REQUIRED")
	}
	if purpose == leaseGeneration {
		if code := remoteQuarantineCode(state); code != "" {
			return SessionError(code)
		}
	}
	return nil
}

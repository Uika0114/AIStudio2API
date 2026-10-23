package api

import "net/http"

// Source-aware clients use a separate route: old binaries cannot silently ignore
// chrome_root and scan the default browser before a capability mismatch is seen.
func (s *server) handleChromeSourceProfiles(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.config.Admin.(ChromeSourceAdmin)
	if !ok {
		writeAdminError(w, http.StatusNotImplemented, "chrome_source_unsupported", "此管理服务未实现独立 Chrome 来源合同")
		return
	}
	values, provided := r.URL.Query()["chrome_root"]
	if !provided || len(values) != 1 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "必须显式选择 Chrome 根；空值表示默认 Chrome")
		return
	}
	discovery, err := admin.ChromeImportProfilesFrom(r.Context(), values[0])
	if err != nil {
		writeAdminUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, discovery)
}

func (s *server) handleImportChromeSource(w http.ResponseWriter, r *http.Request) {
	s.handleChromeImport(w, r, true)
}

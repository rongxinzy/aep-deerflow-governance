package main

import (
	"net/http"
)

// freshAdminOnly revalidates the AEP identity before exposing managed
// knowledge operations. Portal sessions cache roles for ordinary requests;
// managed knowledge administration must not trust those roles after AEP has
// revoked them. Pass the verified identity downstream so handler-level checks
// use the same current role set.
func (p *portal) freshAdminOnly(next func(http.ResponseWriter, *http.Request, identity)) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, cached identity) {
		if cached.Token == "" || p.aep == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "current administrator identity is unavailable"})
			return
		}

		current, err := p.aep.verify(r.Context(), cached.Token)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "current administrator role could not be verified"})
			return
		}
		if current.UserID == "" || current.UserID != cached.UserID {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "current administrator identity is unavailable"})
			return
		}
		if !current.isAdmin(p.cfg) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator role required"})
			return
		}

		next(w, r, current)
	}
}

// GET /api/v1/organization: the full department tree for the workbench org
// view. The botUI org page assembles its tree from me.teams plus the teams
// of listed contacts, which can never include a parent department that has
// no direct members — so the tree needs one authoritative flattened listing
// (id/name/parentId) from the server. Any authenticated employee may read
// it: departments are strictly coarser than the contacts directory that is
// already employee-visible, and the tree must show empty intermediate
// departments for the hierarchy to render at all.
package main

import (
	"net/http"
)

func (p *portal) handleOrganization(w http.ResponseWriter, r *http.Request, ident identity) {
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	directory, err := p.fetchTeams(r.Context(), adminToken)
	if err != nil {
		// Fail closed like the contacts directory: a partial tree would
		// surface as departments flickering in and out of the org view.
		if p.logger != nil {
			p.logger.Warn("organization: team listing failed", "error", err.Error())
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "aep returned an error listing teams"})
		return
	}
	out := make([]map[string]any, 0, len(directory))
	for _, team := range directory {
		entry := map[string]any{"id": team.ID, "name": team.Name}
		if team.ParentID != nil {
			entry["parentId"] = *team.ParentID
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"departments": out})
}

package main

import (
	"fmt"
	"net/http"
)

// decision is the outcome of the "who may create a digital employee"
// policy function — the piece the operator deliberately leaves to the
// business layer.
type decision struct {
	Create        bool   // create the CR immediately
	NeedsApproval bool   // park as a pending request
	HTTPStatus    int    // response code when !Create && !NeedsApproval
	Reason        string // human-readable policy outcome
}

func evaluatePolicy(mode string, isAdmin, isHuman bool, quotaUsed, quota int) decision {
	switch mode {
	case "admin-only":
		if !isAdmin {
			return decision{HTTPStatus: http.StatusForbidden, Reason: "policy mode admin-only: only platform administrators may create digital employees"}
		}
	case "approval", "self-service":
		if !isHuman && !isAdmin {
			return decision{HTTPStatus: http.StatusForbidden, Reason: "only human users may apply for a digital employee"}
		}
	default:
		return decision{HTTPStatus: http.StatusInternalServerError, Reason: "unknown policy mode " + mode}
	}
	if quota > 0 && quotaUsed >= quota {
		return decision{HTTPStatus: http.StatusConflict, Reason: fmt.Sprintf("quota exceeded: %d digital employees already owned (limit %d)", quotaUsed, quota)}
	}
	if mode == "approval" && !isAdmin {
		return decision{NeedsApproval: true, Reason: "policy mode approval: request parked for administrator approval"}
	}
	return decision{Create: true, Reason: "policy allows immediate creation"}
}

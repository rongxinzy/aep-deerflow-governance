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
			p.managedWriteError(w, http.StatusUnauthorized, "ADMIN_IDENTITY_UNAVAILABLE", "当前管理员身份不可用，请重新登录", "")
			return
		}

		current, err := p.aep.verify(r.Context(), cached.Token)
		if err != nil {
			p.managedWriteError(w, http.StatusServiceUnavailable, "ADMIN_ROLE_UNVERIFIED", "无法校验当前管理员角色，请稍后重试", "")
			return
		}
		if current.UserID == "" || current.UserID != cached.UserID {
			p.managedWriteError(w, http.StatusUnauthorized, "ADMIN_IDENTITY_UNAVAILABLE", "当前管理员身份不可用，请重新登录", "")
			return
		}
		if !current.isAdmin(p.cfg) {
			p.managedWriteError(w, http.StatusForbidden, "ADMIN_REQUIRED", "需要管理员权限", "")
			return
		}

		next(w, r, current)
	}
}

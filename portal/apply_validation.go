package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Release-condition and reference validation for the extended apply
// request (POST /api/v1/employees). Every field the console can now send —
// owner override, team, knowledge bases, skills, visibility — is resolved
// against the live AEP / WeKnora catalogs so nothing unvalidated reaches
// the CR. Upstream unavailability surfaces as an error: callers fail
// closed instead of creating an employee with unchecked references.

// violation is one failed check, reported field-anchored so the console
// can point the user at the exact form item.
type violation struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// referenceResolver caches per-id directory lookups for one request. Apply
// and update both resolve references through it; users and teams are looked
// up by id (GET /admin/{users,teams}/{id}), never by listing: the platform
// directory paginates at 200 entries and load-test fleets push real users
// past the first page — a listing-based check calls existing owners "not
// found".
type referenceResolver struct {
	portal    *portal
	ctx       context.Context
	userCache map[string]*aepUser
	teamCache map[string]*aepTeam
}

func newReferenceResolver(p *portal, ctx context.Context) *referenceResolver {
	return &referenceResolver{
		portal:    p,
		ctx:       ctx,
		userCache: map[string]*aepUser{},
		teamCache: map[string]*aepTeam{},
	}
}

func (r *referenceResolver) lookupUser(id string) (*aepUser, bool, error) {
	if cached, ok := r.userCache[id]; ok {
		return cached, cached != nil, nil
	}
	var user aepUser
	found, err := r.portal.aepAdminLookup(r.ctx, "/aep/v1/admin/users/"+url.PathEscape(id), &user)
	if err != nil {
		return nil, false, fmt.Errorf("user %s: %w", id, err)
	}
	if !found {
		r.userCache[id] = nil
		return nil, false, nil
	}
	r.userCache[id] = &user
	return &user, true, nil
}

func (r *referenceResolver) lookupTeam(id string) (*aepTeam, bool, error) {
	if cached, ok := r.teamCache[id]; ok {
		return cached, cached != nil, nil
	}
	var team aepTeam
	found, err := r.portal.aepAdminLookup(r.ctx, "/aep/v1/admin/teams/"+url.PathEscape(id), &team)
	if err != nil {
		return nil, false, fmt.Errorf("team %s: %w", id, err)
	}
	if !found {
		r.teamCache[id] = nil
		return nil, false, nil
	}
	r.teamCache[id] = &team
	return &team, true, nil
}

// applyContext carries the resolved, validated pieces of an apply request.
type applyContext struct {
	OwnerID    string
	OwnerName  string
	KBs        []dev1alpha1.KnowledgeBaseRef
	Skills     []dev1alpha1.SkillRef
	Visibility *dev1alpha1.VisibilitySpec
}

// aepAdminGet performs an authenticated GET against the AEP admin API with
// the bootstrap admin token and decodes the JSON body into out.
func (p *portal) aepAdminGet(ctx context.Context, path string, out any) error {
	token := p.adminToken(ctx)
	if token == "" {
		return fmt.Errorf("admin token unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.aepBaseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", path, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(out)
}

type aepUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
	Kind        string `json:"kind"`
}

type aepTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type aepSkillVersion struct {
	Version string `json:"version"`
	State   string `json:"state"`
}

type aepSkill struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	State    string            `json:"state"`
	Versions []aepSkillVersion `json:"versions"`
}

// latestPublished returns the newest published version string, or "".
func (s aepSkill) latestPublished() string {
	latest := ""
	for _, version := range s.Versions {
		if version.State == "published" {
			latest = version.Version
		}
	}
	return latest
}

func (s aepSkill) hasPublished(version string) bool {
	for _, candidate := range s.Versions {
		if candidate.Version == version && candidate.State == "published" {
			return true
		}
	}
	return false
}

// weknoraBase mirrors the WeKnora knowledge-base listing fields the portal
// needs (id + name for validation and display snapshots).
type weknoraBase struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// listWeKnoraBases lists knowledge bases through the WeKnora tenant API
// key (the same retrieve-scoped key the services status page uses).
func (p *portal) listWeKnoraBases(ctx context.Context) ([]weknoraBase, error) {
	key, err := p.platformSecretKey(ctx, "WEKNORA_API_KEY")
	if err != nil || key == "" {
		return nil, fmt.Errorf("weknora api key not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.knowledgeAPIURL+"/api/v1/knowledge-bases", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-API-Key", key)
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weknora knowledge-bases returned %d", response.StatusCode)
	}
	var payload struct {
		Data []weknoraBase `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

// listAEPSkills lists all skills, following cursor pagination.
func (p *portal) listAEPSkills(ctx context.Context) ([]aepSkill, error) {
	var out []aepSkill
	cursor := ""
	for {
		path := "/aep/v1/admin/skills?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var result struct {
			Skills     []aepSkill `json:"skills"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := p.aepAdminGet(ctx, path, &result); err != nil {
			return nil, err
		}
		out = append(out, result.Skills...)
		if result.NextCursor == "" || len(result.Skills) == 0 {
			return out, nil
		}
		cursor = result.NextCursor
	}
}

// aepAdminLookup performs an admin GET that distinguishes "not found"
// (false, nil) from transport/server failures (false, err).
func (p *portal) aepAdminLookup(ctx context.Context, path string, out any) (bool, error) {
	token := p.adminToken(ctx)
	if token == "" {
		return false, fmt.Errorf("admin token unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.aepBaseURL+path, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
		return true, json.NewDecoder(response.Body).Decode(out)
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("%s returned %d", path, response.StatusCode)
	}
}

// validateApply resolves and validates the extended apply fields. The
// returned violations list every failed check (bad references, missing
// release conditions); a non-nil error means an upstream needed for
// validation was unavailable and the caller must fail closed.
func (p *portal) validateApply(ctx context.Context, ident identity, body applyRequest) (applyContext, []violation, error) {
	resolved := applyContext{}
	var violations []violation
	add := func(field, format string, args ...any) {
		violations = append(violations, violation{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	resolver := newReferenceResolver(p, ctx)
	lookupUser := resolver.lookupUser
	lookupTeam := resolver.lookupTeam

	// Owner: empty means the caller; an override must reference an active
	// human account (privilege is checked in the handler before this).
	switch body.Owner {
	case "":
		resolved.OwnerID = ident.UserID
		resolved.OwnerName = identUsername(ident)
	default:
		user, found, err := lookupUser(body.Owner)
		if err != nil {
			return resolved, nil, err
		}
		switch {
		case !found:
			add("owner", "负责人 %s 不存在", body.Owner)
		case user.Status == "disabled":
			add("owner", "负责人 %s 已停用", user.DisplayName)
		case user.Kind != "" && user.Kind != "human":
			add("owner", "负责人 %s 不是真人账号", user.DisplayName)
		default:
			resolved.OwnerID = user.ID
			resolved.OwnerName = user.DisplayName
			if resolved.OwnerName == "" {
				resolved.OwnerName = user.Username
			}
		}
	}

	// Team is a required release condition and must reference a real team.
	if body.Team == "" {
		add("team", "所属团队为必填发布条件")
	} else if _, found, err := lookupTeam(body.Team); err != nil {
		return resolved, nil, err
	} else if !found {
		add("team", "团队 %s 不存在", body.Team)
	}

	// Knowledge bases: nil input keeps the legacy unrestricted policy; a
	// non-nil list (even empty) is an explicit policy. Only non-empty lists
	// consult WeKnora — an explicit deny-all has no ids to validate, so a
	// WeKnora outage must not block creating knowledge-less employees.
	if body.KnowledgeBases != nil {
		resolved.KBs = []dev1alpha1.KnowledgeBaseRef{}
		if len(body.KnowledgeBases) > 0 {
			bases, err := p.listWeKnoraBases(ctx)
			if err != nil {
				return resolved, nil, fmt.Errorf("knowledge bases: %w", err)
			}
			for index, id := range body.KnowledgeBases {
				matched := false
				for _, base := range bases {
					if base.ID == id {
						resolved.KBs = append(resolved.KBs, dev1alpha1.KnowledgeBaseRef{ID: base.ID, Name: base.Name})
						matched = true
						break
					}
				}
				if !matched {
					add(fmt.Sprintf("knowledgeBases[%d]", index), "知识库 %s 不存在", id)
				}
			}
		}
	}

	// Skills: must exist, be active, and pin a published version (an empty
	// version auto-pins the latest published one).
	if len(body.Skills) > 0 {
		skills, err := p.listAEPSkills(ctx)
		if err != nil {
			return resolved, nil, fmt.Errorf("skills: %w", err)
		}
		resolved.Skills = []dev1alpha1.SkillRef{}
		for index, input := range body.Skills {
			field := fmt.Sprintf("skills[%d].id", index)
			var matched *aepSkill
			for i := range skills {
				if skills[i].ID == input.ID {
					matched = &skills[i]
					break
				}
			}
			switch {
			case matched == nil:
				add(field, "技能 %s 不存在", input.ID)
			case matched.State != "active":
				add(field, "技能 %s 未启用", matched.Name)
			case input.Version == "" && matched.latestPublished() == "":
				add(field, "技能 %s 没有已发布版本", matched.Name)
			case input.Version != "" && !matched.hasPublished(input.Version):
				add(field, "技能 %s 的版本 %s 未发布", matched.Name, input.Version)
			default:
				version := input.Version
				if version == "" {
					version = matched.latestPublished()
				}
				resolved.Skills = append(resolved.Skills, dev1alpha1.SkillRef{ID: matched.ID, Name: matched.Name, Version: version})
			}
		}
	}

	// Visibility: explicit and never defaulted to all-users.
	switch {
	case body.Visibility == nil:
		add("visibility", "开放范围为必填发布条件，不能默认全员开放")
	case body.Visibility.Mode == "all":
		resolved.Visibility = &dev1alpha1.VisibilitySpec{Mode: "all"}
	case body.Visibility.Mode == "restricted":
		visibility := &dev1alpha1.VisibilitySpec{Mode: "restricted", Teams: []dev1alpha1.SubjectRef{}, Users: []dev1alpha1.SubjectRef{}}
		for index, id := range body.Visibility.Teams {
			team, found, err := lookupTeam(id)
			if err != nil {
				return resolved, nil, err
			}
			if found {
				visibility.Teams = append(visibility.Teams, dev1alpha1.SubjectRef{ID: team.ID, Name: team.Name})
			} else {
				add(fmt.Sprintf("visibility.teams[%d]", index), "团队 %s 不存在", id)
			}
		}
		for index, id := range body.Visibility.Users {
			user, found, err := lookupUser(id)
			if err != nil {
				return resolved, nil, err
			}
			if found {
				name := user.DisplayName
				if name == "" {
					name = user.Username
				}
				visibility.Users = append(visibility.Users, dev1alpha1.SubjectRef{ID: user.ID, Name: name})
			} else {
				add(fmt.Sprintf("visibility.users[%d]", index), "用户 %s 不存在", id)
			}
		}
		if len(body.Visibility.Teams) == 0 && len(body.Visibility.Users) == 0 {
			add("visibility", "指定范围至少需要包含一个团队或用户")
		}
		resolved.Visibility = visibility
	default:
		add("visibility", "开放范围模式必须是 all 或 restricted")
	}

	// Description: required release condition, bounded length.
	if len([]rune(body.Description)) == 0 {
		add("description", "用途说明为必填发布条件")
	} else if len([]rune(body.Description)) > 500 {
		add("description", "用途说明不能超过 500 字")
	}

	// Note (补充说明): optional applicant context, bounded length. The
	// bound applies to the trimmed value — that is what renderSpec stores,
	// so whitespace padding must not reject a note that lands in bounds.
	if len([]rune(strings.TrimSpace(body.Note))) > 500 {
		add("note", "补充说明不能超过 500 字")
	}

	return resolved, violations, nil
}

// validateUpdate resolves and validates only the fields an update request
// actually carries — absent means unchanged, and an untouched field never
// consults an upstream (switching a dead model must not be blocked by an
// unrelated stale reference). The per-field rule set mirrors validateApply;
// resolved values land in the context only for provided-and-valid fields,
// so the handler can tell "keep current" (zero value) from "replace with
// this" for every input shape it knows from the body itself.
func (p *portal) validateUpdate(ctx context.Context, ident identity, body updateRequest) (applyContext, []violation, error) {
	resolved := applyContext{}
	var violations []violation
	add := func(field, format string, args ...any) {
		violations = append(violations, violation{Field: field, Message: fmt.Sprintf(format, args...)})
	}
	resolver := newReferenceResolver(p, ctx)

	if body.Owner != nil {
		switch *body.Owner {
		case "":
			add("owner", "负责人不能为空")
		default:
			user, found, err := resolver.lookupUser(*body.Owner)
			if err != nil {
				return resolved, nil, err
			}
			switch {
			case !found:
				add("owner", "负责人 %s 不存在", *body.Owner)
			case user.Status == "disabled":
				add("owner", "负责人 %s 已停用", user.DisplayName)
			case user.Kind != "" && user.Kind != "human":
				add("owner", "负责人 %s 不是真人账号", user.DisplayName)
			default:
				resolved.OwnerID = user.ID
				resolved.OwnerName = user.DisplayName
				if resolved.OwnerName == "" {
					resolved.OwnerName = user.Username
				}
			}
		}
	}

	if body.Team != nil {
		if *body.Team == "" {
			add("team", "所属团队为必填发布条件")
		} else if _, found, err := resolver.lookupTeam(*body.Team); err != nil {
			return resolved, nil, err
		} else if !found {
			add("team", "团队 %s 不存在", *body.Team)
		}
	}

	if body.Description != nil {
		switch runes := len([]rune(*body.Description)); {
		case runes == 0:
			add("description", "用途说明为必填发布条件")
		case runes > 500:
			add("description", "用途说明不能超过 500 字")
		}
	}

	// Same knowledge semantics as apply: an explicit empty list is a valid
	// deny-all policy with no ids to validate, so WeKnora stays out of the
	// loop and an outage cannot block hardening an employee to zero bases.
	if body.KnowledgeBases != nil {
		resolved.KBs = []dev1alpha1.KnowledgeBaseRef{}
		if len(body.KnowledgeBases) > 0 {
			bases, err := p.listWeKnoraBases(ctx)
			if err != nil {
				return resolved, nil, fmt.Errorf("knowledge bases: %w", err)
			}
			for index, id := range body.KnowledgeBases {
				matched := false
				for _, base := range bases {
					if base.ID == id {
						resolved.KBs = append(resolved.KBs, dev1alpha1.KnowledgeBaseRef{ID: base.ID, Name: base.Name})
						matched = true
						break
					}
				}
				if !matched {
					add(fmt.Sprintf("knowledgeBases[%d]", index), "知识库 %s 不存在", id)
				}
			}
		}
	}

	if body.Skills != nil {
		resolved.Skills = []dev1alpha1.SkillRef{}
		if len(body.Skills) > 0 {
			skills, err := p.listAEPSkills(ctx)
			if err != nil {
				return resolved, nil, fmt.Errorf("skills: %w", err)
			}
			for index, input := range body.Skills {
				field := fmt.Sprintf("skills[%d].id", index)
				var matched *aepSkill
				for i := range skills {
					if skills[i].ID == input.ID {
						matched = &skills[i]
						break
					}
				}
				switch {
				case matched == nil:
					add(field, "技能 %s 不存在", input.ID)
				case matched.State != "active":
					add(field, "技能 %s 未启用", matched.Name)
				case input.Version == "" && matched.latestPublished() == "":
					add(field, "技能 %s 没有已发布版本", matched.Name)
				case input.Version != "" && !matched.hasPublished(input.Version):
					add(field, "技能 %s 的版本 %s 未发布", matched.Name, input.Version)
				default:
					version := input.Version
					if version == "" {
						version = matched.latestPublished()
					}
					resolved.Skills = append(resolved.Skills, dev1alpha1.SkillRef{ID: matched.ID, Name: matched.Name, Version: version})
				}
			}
		}
	}

	if body.Visibility != nil {
		switch body.Visibility.Mode {
		case "all":
			resolved.Visibility = &dev1alpha1.VisibilitySpec{Mode: "all"}
		case "restricted":
			visibility := &dev1alpha1.VisibilitySpec{Mode: "restricted", Teams: []dev1alpha1.SubjectRef{}, Users: []dev1alpha1.SubjectRef{}}
			for index, id := range body.Visibility.Teams {
				team, found, err := resolver.lookupTeam(id)
				if err != nil {
					return resolved, nil, err
				}
				if found {
					visibility.Teams = append(visibility.Teams, dev1alpha1.SubjectRef{ID: team.ID, Name: team.Name})
				} else {
					add(fmt.Sprintf("visibility.teams[%d]", index), "团队 %s 不存在", id)
				}
			}
			for index, id := range body.Visibility.Users {
				user, found, err := resolver.lookupUser(id)
				if err != nil {
					return resolved, nil, err
				}
				if found {
					name := user.DisplayName
					if name == "" {
						name = user.Username
					}
					visibility.Users = append(visibility.Users, dev1alpha1.SubjectRef{ID: user.ID, Name: name})
				} else {
					add(fmt.Sprintf("visibility.users[%d]", index), "用户 %s 不存在", id)
				}
			}
			if len(body.Visibility.Teams) == 0 && len(body.Visibility.Users) == 0 {
				add("visibility", "指定范围至少需要包含一个团队或用户")
			}
			resolved.Visibility = visibility
		default:
			add("visibility", "开放范围模式必须是 all 或 restricted")
		}
	}

	return resolved, violations, nil
}

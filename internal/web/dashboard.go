package web

import (
	"errors"
	"net/http"
	"net/url"

	"sherpa/internal/web/registryclient"
)

type dashboardPageView struct {
	Login          string
	CSRFToken      string
	Follows        []dashboardFollowView
	Updates        []dashboardUpdateView
	NextFollowsURL string
	NextUpdatesURL string
}
type dashboardFollowView struct {
	Ref            string
	Summary        string
	LatestVersion  int
	FollowerCount  int
	UnfollowAction string
}
type dashboardUpdateView struct {
	Ref           string
	Version       int
	SeenVersion   int
	Changelog     string
	PublishedISO  string
	PublishedText string
	SeenAction    string
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	followsCursor, ok := boundedQueryValue(r.URL.Query(), "follows_cursor", 512)
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	updatesCursor, ok := boundedQueryValue(r.URL.Query(), "updates_cursor", 512)
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	identity, token, err := s.sessionIdentity(w, r)
	if err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	follows, err := s.authRegistry.Follows(r.Context(), token, 50, followsCursor)
	if err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	updates, err := s.authRegistry.Updates(r.Context(), token, 50, updatesCursor)
	if err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	view := &dashboardPageView{Login: identity.Login, CSRFToken: csrfToken(r)}
	for _, f := range follows.Follows {
		view.Follows = append(view.Follows, dashboardFollowView{Ref: "@" + f.Owner + "/" + f.Name, Summary: f.Summary, LatestVersion: f.LatestVersion, FollowerCount: f.FollowerCount, UnfollowAction: stackPath(f.Owner, f.Name) + "/unfollow"})
	}
	for _, u := range updates.Updates {
		iso, text := displayTime(u.PublishedAt)
		view.Updates = append(view.Updates, dashboardUpdateView{Ref: "@" + u.Owner + "/" + u.Name, Version: u.Version, SeenVersion: u.SeenVersion, Changelog: u.Changelog, PublishedISO: iso, PublishedText: text, SeenAction: "/dashboard/seen/" + u.Owner + "/" + u.Name})
	}
	if follows.NextCursor != "" {
		view.NextFollowsURL = dashboardURL(follows.NextCursor, updatesCursor)
	}
	if updates.NextCursor != "" {
		view.NextUpdatesURL = dashboardURL(followsCursor, updates.NextCursor)
	}
	data := pageData{Title: "Dashboard", Robots: "noindex,nofollow", Auth: authView{Enabled: true, SignedIn: true, Login: identity.Login, CSRFToken: csrfToken(r)}, DashboardPage: view}
	w.Header().Set("X-Robots-Tag", "noindex")
	if err := s.renderer.render(w, http.StatusOK, data); err != nil {
		writeFallbackError(w)
	}
}

func dashboardURL(followsCursor, updatesCursor string) string {
	values := url.Values{}
	if followsCursor != "" {
		values.Set("follows_cursor", followsCursor)
	}
	if updatesCursor != "" {
		values.Set("updates_cursor", updatesCursor)
	}
	return (&url.URL{Path: "/dashboard", RawQuery: values.Encode()}).String()
}

func (s *server) handlePrivateError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, registryclient.ErrUnauthorized) {
		clearAuthCookies(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.renderRegistryError(w, err)
}

func (s *server) handleFollowMutation(w http.ResponseWriter, r *http.Request, owner, name string, unfollow bool) {
	if !validSegment(owner) || !validSegment(name) {
		s.renderError(w, http.StatusNotFound)
		return
	}
	if !s.validMutation(w, r) {
		s.renderError(w, http.StatusForbidden)
		return
	}
	_, token, err := s.sessionIdentity(w, r)
	if err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	if unfollow {
		err = s.authRegistry.Unfollow(r.Context(), token, owner, name)
	} else {
		_, err = s.authRegistry.Follow(r.Context(), token, owner, name)
	}
	if err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	http.Redirect(w, r, stackPath(owner, name), http.StatusSeeOther)
}

func (s *server) handleSeenMutation(w http.ResponseWriter, r *http.Request, owner, name string) {
	if !validSegment(owner) || !validSegment(name) {
		s.renderError(w, http.StatusNotFound)
		return
	}
	if !s.validMutation(w, r) {
		s.renderError(w, http.StatusForbidden)
		return
	}
	versions := r.PostForm["version"]
	if len(versions) != 1 {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	version, ok := parsePositiveInteger(versions[0])
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	_, token, err := s.sessionIdentity(w, r)
	if err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	if _, err = s.authRegistry.MarkSeen(r.Context(), token, owner, name, version); err != nil {
		s.handlePrivateError(w, r, err)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

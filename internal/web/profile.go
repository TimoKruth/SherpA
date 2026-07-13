package web

import (
	"net/http"
	"strconv"
)

type profilePageView struct {
	Handle            string
	TotalStackFollows int
	Stacks            searchPageView
	PreviousURL       string
	NextURL           string
}

func (s *server) handleProfile(w http.ResponseWriter, r *http.Request, handle string) {
	if !validSegment(handle) {
		s.renderError(w, http.StatusNotFound)
		return
	}
	pageNumber, ok := parsePage(r.URL.Query(), "page")
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	page, ok := pageFromNumber(pageNumber, searchPageSize)
	if !ok {
		s.renderError(w, http.StatusBadRequest)
		return
	}
	profile, err := s.registry.GetUser(r.Context(), handle, page)
	if err != nil {
		s.renderRegistryError(w, err)
		return
	}
	view := &profilePageView{Handle: profile.Handle, TotalStackFollows: profile.TotalStackFollows, Stacks: searchPageView{Rows: searchRows(profile.Stacks)}}
	if pageNumber > 1 {
		view.PreviousURL = profilePath(handle, pageNumber-1)
	}
	if profile.NextOffset != nil {
		view.NextURL = profilePath(handle, pageNumber+1)
	}
	canonical := "/users/" + handle
	data := pageData{Title: "@" + handle, Description: "Stacks published by @" + handle, Robots: "index,follow", CanonicalURL: s.canonicalURL(canonical), OpenGraphURL: s.canonicalURL(canonical), OpenGraphTitle: "@" + handle + " · SherpA", Auth: s.authViewFor(w, r), ProfilePage: view}
	if err := s.renderer.render(w, http.StatusOK, data); err != nil {
		writeFallbackError(w)
	}
}

func profilePath(handle string, page int) string {
	if page <= 1 {
		return "/users/" + handle
	}
	return "/users/" + handle + "?page=" + strconv.Itoa(page)
}

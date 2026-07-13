package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/store"
)

const (
	defaultSocialPageSize = 25
	maxSocialPageSize     = 50
	socialBodyLimit       = 8 << 10
	maxSocialCursorBytes  = 512
)

type followResponse struct {
	Ref               string   `json:"ref"`
	Owner             string   `json:"owner"`
	Name              string   `json:"name"`
	Summary           string   `json:"summary"`
	Harness           string   `json:"harness"`
	Tags              []string `json:"tags"`
	LatestVersion     int      `json:"latest_version"`
	LatestGitTag      string   `json:"latest_git_tag"`
	LatestTrustTier   string   `json:"latest_trust_tier"`
	LatestPublishedAt string   `json:"latest_published_at"`
	LastSeenVersion   int      `json:"last_seen_version"`
	FollowerCount     int      `json:"follower_count"`
	FollowedAt        string   `json:"followed_at"`
}

type updateResponse struct {
	Ref         string `json:"ref"`
	Owner       string `json:"owner"`
	Name        string `json:"name"`
	Version     int    `json:"version"`
	GitTag      string `json:"git_tag"`
	Changelog   string `json:"changelog"`
	TrustTier   string `json:"trust_tier"`
	PublishedAt string `json:"published_at"`
	SeenVersion int    `json:"seen_version"`
}

type followCursor struct {
	Owner string `json:"o"`
	Name  string `json:"n"`
}

type updateCursor struct {
	EventID int64 `json:"e"`
}

func (s *server) personalIdentity(w http.ResponseWriter, r *http.Request) (store.SessionIdentity, bool) {
	w.Header().Set("Cache-Control", "no-store")
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.SessionIdentity{}, false
	}
	token, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || token == "" || strings.TrimSpace(token) != token || s.isAdminToken(token) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.SessionIdentity{}, false
	}
	identity, err := s.store.SessionIdentity(r.Context(), registryauth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.SessionIdentity{}, false
	}
	if err != nil {
		s.logger.Printf("personal auth failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return store.SessionIdentity{}, false
	}
	if !identity.Purpose.Valid() {
		s.logger.Printf("personal auth invalid session purpose")
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return store.SessionIdentity{}, false
	}
	return identity, true
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"login": identity.Login, "purpose": string(identity.Purpose)})
}

func (s *server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeSession(r.Context(), identity.SessionID); err != nil {
		s.logger.Printf("revoke session failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleFollow(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	owner, name, ok := socialRef(w, r)
	if !ok {
		return
	}
	follow, err := s.store.FollowStack(r.Context(), identity.UserID, owner, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "stack not found")
		return
	}
	if err != nil {
		s.logger.Printf("follow stack failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, followView(follow))
}

func (s *server) handleUnfollow(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	owner, name, ok := socialRef(w, r)
	if !ok {
		return
	}
	if err := s.store.UnfollowStack(r.Context(), identity.UserID, owner, name); err != nil {
		s.logger.Printf("unfollow stack failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListFollows(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	limit, cursor, err := parseFollowPage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination")
		return
	}
	follows, err := s.store.ListFollows(r.Context(), identity.UserID, store.FollowPage{
		Limit: limit + 1, AfterOwner: cursor.Owner, AfterName: cursor.Name,
	})
	if err != nil {
		s.logger.Printf("list follows failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	nextCursor := ""
	if len(follows) > limit {
		follows = follows[:limit]
		last := follows[len(follows)-1]
		nextCursor = encodeSocialCursor(followCursor{Owner: last.Owner, Name: last.Name})
	}
	views := make([]followResponse, 0, len(follows))
	for _, follow := range follows {
		views = append(views, followView(follow))
	}
	writeJSON(w, http.StatusOK, map[string]any{"follows": views, "next_cursor": nextCursor})
}

func (s *server) handleListUpdates(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	limit, cursor, err := parseUpdatePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination")
		return
	}
	updates, err := s.store.ListUpdates(r.Context(), identity.UserID, store.UpdatePage{
		Limit: limit + 1, BeforeEventID: cursor.EventID,
	})
	if err != nil {
		s.logger.Printf("list updates failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	nextCursor := ""
	if len(updates) > limit {
		updates = updates[:limit]
		nextCursor = encodeSocialCursor(updateCursor{EventID: updates[len(updates)-1].EventID})
	}
	views := make([]updateResponse, 0, len(updates))
	for _, update := range updates {
		views = append(views, updateView(update))
	}
	writeJSON(w, http.StatusOK, map[string]any{"updates": views, "next_cursor": nextCursor})
}

func (s *server) handleMarkSeen(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	owner, name, ok := socialRef(w, r)
	if !ok {
		return
	}
	var body struct {
		Version int `json:"version"`
	}
	if err := decodeSocialJSON(w, r, &body); err != nil || body.Version <= 0 {
		if err == nil {
			writeError(w, http.StatusBadRequest, "invalid version")
		}
		return
	}
	follow, err := s.store.MarkSeen(r.Context(), identity.UserID, owner, name, body.Version)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "follow or version not found")
		return
	}
	if err != nil {
		s.logger.Printf("mark follow seen failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, followView(follow))
}

func (s *server) handlePutTrial(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.personalIdentity(w, r)
	if !ok {
		return
	}
	owner, name, ok := socialRef(w, r)
	if !ok {
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version <= 0 {
		writeError(w, http.StatusBadRequest, "invalid version")
		return
	}
	var body struct {
		Verdict store.Verdict `json:"verdict"`
	}
	if err := decodeSocialJSON(w, r, &body); err != nil {
		return
	}
	if !body.Verdict.Valid() {
		writeError(w, http.StatusBadRequest, "invalid verdict")
		return
	}
	err = s.store.PutTrialFeedback(r.Context(), identity.UserID, owner, name, version, body.Verdict)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		s.logger.Printf("put trial feedback failed error_type=%T", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func socialRef(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	owner, name := r.PathValue("owner"), r.PathValue("name")
	if !validPublishSegment(owner) || !validPublishSegment(name) {
		writeError(w, http.StatusBadRequest, "invalid owner or name")
		return "", "", false
	}
	return owner, name, true
}

func decodeSocialJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return errors.New("invalid content type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, socialBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON")
		}
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON")
		}
		return err
	}
	return nil
}

func parseFollowPage(r *http.Request) (int, followCursor, error) {
	limit, raw, err := parseSocialPage(r)
	if err != nil || raw == "" {
		return limit, followCursor{}, err
	}
	var cursor followCursor
	if err := decodeSocialCursor(raw, &cursor); err != nil || !validPublishSegment(cursor.Owner) || !validPublishSegment(cursor.Name) {
		return 0, followCursor{}, errors.New("invalid cursor")
	}
	return limit, cursor, nil
}

func parseUpdatePage(r *http.Request) (int, updateCursor, error) {
	limit, raw, err := parseSocialPage(r)
	if err != nil || raw == "" {
		return limit, updateCursor{}, err
	}
	var cursor updateCursor
	if err := decodeSocialCursor(raw, &cursor); err != nil || cursor.EventID <= 0 {
		return 0, updateCursor{}, errors.New("invalid cursor")
	}
	return limit, cursor, nil
}

func parseSocialPage(r *http.Request) (int, string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, "", errors.New("invalid query")
	}
	limit := defaultSocialPageSize
	if values, ok := query["limit"]; ok {
		if len(values) != 1 {
			return 0, "", errors.New("invalid limit")
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed <= 0 || parsed > maxSocialPageSize {
			return 0, "", errors.New("invalid limit")
		}
		limit = parsed
	}
	cursors := query["cursor"]
	if len(cursors) > 1 {
		return 0, "", errors.New("invalid cursor")
	}
	if len(cursors) == 0 {
		return limit, "", nil
	}
	if len(cursors[0]) == 0 || len(cursors[0]) > maxSocialCursorBytes {
		return 0, "", errors.New("invalid cursor")
	}
	return limit, cursors[0], nil
}

func encodeSocialCursor(value any) string {
	encoded, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeSocialCursor(raw string, dst any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) > maxSocialCursorBytes {
		return errors.New("invalid cursor")
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid cursor")
	}
	return nil
}

func followView(follow store.Follow) followResponse {
	return followResponse{
		Ref: "@" + follow.Owner + "/" + follow.Name, Owner: follow.Owner, Name: follow.Name,
		Summary: follow.Summary, Harness: follow.Harness, Tags: tagsOrEmpty(follow.Tags),
		LatestVersion: follow.LatestVersion, LatestGitTag: follow.LatestGitTag,
		LatestTrustTier: follow.LatestTrustTier, LatestPublishedAt: formatTime(follow.LatestPublishedAt),
		LastSeenVersion: follow.LastSeenVersion, FollowerCount: follow.FollowerCount,
		FollowedAt: formatTime(follow.CreatedAt),
	}
}

func updateView(update store.Update) updateResponse {
	return updateResponse{
		Ref: "@" + update.Owner + "/" + update.Name, Owner: update.Owner, Name: update.Name,
		Version: update.Version, GitTag: update.GitTag, Changelog: update.Changelog,
		TrustTier: update.TrustTier, PublishedAt: formatTime(update.PublishedAt), SeenVersion: update.SeenVersion,
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	registryauth "sherpa/internal/registry/auth"
	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

type socialFakeStore struct {
	*fakeStore
	identities     map[string]store.SessionIdentity
	follow         store.Follow
	followErr      error
	follows        []store.Follow
	updates        []store.Update
	lastFollowPage store.FollowPage
	lastUpdatePage store.UpdatePage
	lastUserID     int64
	lastOwner      string
	lastName       string
	lastVersion    int
	lastVerdict    store.Verdict
	revokeCalls    int
	unfollowCalls  int
	markSeenCalls  int
	trialCalls     int
}

func newSocialFakeStore() *socialFakeStore {
	return &socialFakeStore{fakeStore: newFakeStore(), identities: map[string]store.SessionIdentity{}}
}

func (f *socialFakeStore) SessionIdentity(_ context.Context, hash string) (store.SessionIdentity, error) {
	identity, ok := f.identities[hash]
	if !ok {
		return store.SessionIdentity{}, store.ErrNotFound
	}
	return identity, nil
}

func (f *socialFakeStore) RevokeSession(_ context.Context, sessionID int64) error {
	f.revokeCalls++
	f.lastUserID = sessionID
	return nil
}

func (f *socialFakeStore) FollowStack(_ context.Context, userID int64, owner, name string) (store.Follow, error) {
	f.lastUserID, f.lastOwner, f.lastName = userID, owner, name
	return f.follow, f.followErr
}

func (f *socialFakeStore) GetFollow(_ context.Context, userID int64, owner, name string) (store.Follow, error) {
	f.lastUserID, f.lastOwner, f.lastName = userID, owner, name
	return f.follow, f.followErr
}

func (f *socialFakeStore) UnfollowStack(_ context.Context, userID int64, owner, name string) error {
	f.unfollowCalls++
	f.lastUserID, f.lastOwner, f.lastName = userID, owner, name
	return f.followErr
}

func (f *socialFakeStore) ListFollows(_ context.Context, userID int64, page store.FollowPage) ([]store.Follow, error) {
	f.lastUserID, f.lastFollowPage = userID, page
	return f.follows, f.followErr
}

func (f *socialFakeStore) ListUpdates(_ context.Context, userID int64, page store.UpdatePage) ([]store.Update, error) {
	f.lastUserID, f.lastUpdatePage = userID, page
	return f.updates, f.followErr
}

func (f *socialFakeStore) MarkSeen(_ context.Context, userID int64, owner, name string, version int) (store.Follow, error) {
	f.markSeenCalls++
	f.lastUserID, f.lastOwner, f.lastName, f.lastVersion = userID, owner, name, version
	return f.follow, f.followErr
}

func (f *socialFakeStore) PutTrialFeedback(_ context.Context, userID int64, owner, name string, version int, verdict store.Verdict) error {
	f.trialCalls++
	f.lastUserID, f.lastOwner, f.lastName, f.lastVersion, f.lastVerdict = userID, owner, name, version, verdict
	return f.followErr
}

func socialHandler(t *testing.T, st *socialFakeStore, admin string) http.Handler {
	t.Helper()
	return New(st, content.NewBareGit(t.TempDir()), admin, &registryauth.FakeGitHubClient{})
}

func socialRequest(method, path, token string, body string) *http.Request {
	request := httptest.NewRequest(method, "http://registry.test"+path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestPersonalAuthMatrixAndSessionRevocation(t *testing.T) {
	for _, purpose := range []store.SessionPurpose{store.SessionCLI, store.SessionWeb} {
		t.Run(string(purpose), func(t *testing.T) {
			st := newSocialFakeStore()
			st.identities[registryauth.HashToken("user-token")] = store.SessionIdentity{
				SessionID: 8, UserID: 7, Login: "alice", Purpose: purpose,
			}
			handler := socialHandler(t, st, "admin-token")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me", "user-token", ""))
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"login":"alice"`) || !strings.Contains(rr.Body.String(), `"purpose":"`+string(purpose)+`"`) {
				t.Fatalf("me = %d %s", rr.Code, rr.Body.String())
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", rr.Header().Get("Cache-Control"))
			}

			rr = httptest.NewRecorder()
			handler.ServeHTTP(rr, socialRequest(http.MethodDelete, "/v1/me/session", "user-token", ""))
			if rr.Code != http.StatusNoContent || st.revokeCalls != 1 || st.lastUserID != 8 {
				t.Fatalf("revoke = %d calls=%d id=%d", rr.Code, st.revokeCalls, st.lastUserID)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "invalid", token: "invalid"},
		{name: "admin rejected", token: "admin-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newSocialFakeStore()
			rr := httptest.NewRecorder()
			socialHandler(t, st, "admin-token").ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me", tc.token, ""))
			if rr.Code != http.StatusUnauthorized || rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response = %d cache=%q body=%s", rr.Code, rr.Header().Get("Cache-Control"), rr.Body.String())
			}
		})
	}

	st := newSocialFakeStore()
	st.identities[registryauth.HashToken("user-token")] = store.SessionIdentity{UserID: 7, Login: "alice", Purpose: store.SessionCLI}
	req := socialRequest(http.MethodGet, "/v1/me", "user-token", "")
	req.Header.Add("Authorization", "Bearer user-token")
	rr := httptest.NewRecorder()
	socialHandler(t, st, "").ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization status = %d", rr.Code)
	}
}

func TestFollowAndListPagination(t *testing.T) {
	st := newSocialFakeStore()
	st.identities[registryauth.HashToken("token")] = store.SessionIdentity{UserID: 7, Login: "bob", Purpose: store.SessionCLI}
	st.follow = store.Follow{
		Owner: "alice", Name: "reviewer", Summary: "Review code", Harness: "codex", Tags: []string{"review"},
		LatestVersion: 2, LatestGitTag: "v2", LatestTrustTier: "linked", LastSeenVersion: 1,
		FollowerCount: 3, LatestPublishedAt: testTime(2), CreatedAt: testTime(1),
	}
	handler := socialHandler(t, st, "")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodPut, "/v1/me/follows/alice/reviewer", "token", ""))
	if rr.Code != http.StatusOK || st.lastUserID != 7 || st.lastOwner != "alice" || st.lastName != "reviewer" {
		t.Fatalf("follow = %d %#v", rr.Code, st)
	}
	if !strings.Contains(rr.Body.String(), `"ref":"@alice/reviewer"`) || !strings.Contains(rr.Body.String(), `"follower_count":3`) {
		t.Fatalf("follow body = %s", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me/follows/alice/reviewer", "token", ""))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ref":"@alice/reviewer"`) {
		t.Fatalf("get follow = %d %s", rr.Code, rr.Body.String())
	}
	st.followErr = store.ErrNotFound
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me/follows/alice/missing", "token", ""))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing follow = %d %s", rr.Code, rr.Body.String())
	}
	st.followErr = nil

	st.follows = []store.Follow{
		{Owner: "alice", Name: "one"}, {Owner: "alice", Name: "two"}, {Owner: "bob", Name: "three"},
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me/follows?limit=2", "token", ""))
	if rr.Code != http.StatusOK || st.lastFollowPage.Limit != 3 {
		t.Fatalf("list = %d page=%#v body=%s", rr.Code, st.lastFollowPage, rr.Body.String())
	}
	var page struct {
		Follows    []followResponse `json:"follows"`
		NextCursor string           `json:"next_cursor"`
	}
	decodeJSON(t, rr, &page)
	if len(page.Follows) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %#v", page)
	}

	st.follows = nil
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me/follows?limit=2&cursor="+page.NextCursor, "token", ""))
	if rr.Code != http.StatusOK || st.lastFollowPage.AfterOwner != "alice" || st.lastFollowPage.AfterName != "two" {
		t.Fatalf("cursor page = %d %#v", rr.Code, st.lastFollowPage)
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodDelete, "/v1/me/follows/alice/reviewer", "token", ""))
	if rr.Code != http.StatusNoContent || st.unfollowCalls != 1 {
		t.Fatalf("unfollow = %d calls=%d", rr.Code, st.unfollowCalls)
	}
}

func TestUpdatePaginationSeenAndTrialBodies(t *testing.T) {
	st := newSocialFakeStore()
	st.identities[registryauth.HashToken("token")] = store.SessionIdentity{UserID: 7, Login: "bob", Purpose: store.SessionWeb}
	st.follow = store.Follow{Owner: "alice", Name: "reviewer", LatestVersion: 3, LastSeenVersion: 2}
	st.updates = []store.Update{
		{EventID: 12, Owner: "alice", Name: "reviewer", Version: 3, SeenVersion: 1},
		{EventID: 11, Owner: "alice", Name: "reviewer", Version: 2, SeenVersion: 1},
	}
	handler := socialHandler(t, st, "")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me/updates?limit=1", "token", ""))
	var page struct {
		Updates    []updateResponse `json:"updates"`
		NextCursor string           `json:"next_cursor"`
	}
	decodeJSON(t, rr, &page)
	if rr.Code != http.StatusOK || len(page.Updates) != 1 || page.NextCursor == "" || st.lastUpdatePage.Limit != 2 {
		t.Fatalf("updates = %d %#v page=%#v", rr.Code, page, st.lastUpdatePage)
	}
	st.updates = nil
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodGet, "/v1/me/updates?cursor="+page.NextCursor, "token", ""))
	if rr.Code != http.StatusOK || st.lastUpdatePage.BeforeEventID != 12 {
		t.Fatalf("update cursor = %d %#v", rr.Code, st.lastUpdatePage)
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodPut, "/v1/me/follows/alice/reviewer/seen", "token", `{"version":3}`))
	if rr.Code != http.StatusOK || st.markSeenCalls != 1 || st.lastVersion != 3 {
		t.Fatalf("seen = %d calls=%d version=%d", rr.Code, st.markSeenCalls, st.lastVersion)
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, socialRequest(http.MethodPut, "/v1/me/trials/alice/reviewer/3", "token", `{"verdict":"keep_with_notes"}`))
	if rr.Code != http.StatusNoContent || st.trialCalls != 1 || st.lastVerdict != store.VerdictKeepWithNotes {
		t.Fatalf("trial = %d calls=%d verdict=%q", rr.Code, st.trialCalls, st.lastVerdict)
	}

	for _, tc := range []struct {
		name        string
		path        string
		contentType string
		body        string
		want        int
	}{
		{name: "unknown seen field", path: "/v1/me/follows/alice/reviewer/seen", contentType: "application/json", body: `{"version":3,"notes":"secret"}`, want: 400},
		{name: "invalid verdict", path: "/v1/me/trials/alice/reviewer/3", contentType: "application/json", body: `{"verdict":"maybe"}`, want: 400},
		{name: "trial notes rejected", path: "/v1/me/trials/alice/reviewer/3", contentType: "application/json", body: `{"verdict":"keep","notes":"secret"}`, want: 400},
		{name: "wrong content type", path: "/v1/me/follows/alice/reviewer/seen", contentType: "text/plain", body: `{"version":3}`, want: 415},
		{name: "multiple values", path: "/v1/me/follows/alice/reviewer/seen", contentType: "application/json", body: `{"version":3}{"version":4}`, want: 400},
		{name: "oversized", path: "/v1/me/follows/alice/reviewer/seen", contentType: "application/json", body: `{"version":3}` + strings.Repeat(" ", socialBodyLimit), want: 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeSeen, beforeTrial := st.markSeenCalls, st.trialCalls
			req := socialRequest(http.MethodPut, tc.path, "token", tc.body)
			req.Header.Set("Content-Type", tc.contentType)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.want || st.markSeenCalls != beforeSeen || st.trialCalls != beforeTrial {
				t.Fatalf("response=%d body=%s seen=%d trial=%d", rr.Code, rr.Body.String(), st.markSeenCalls, st.trialCalls)
			}
		})
	}
}

func TestSocialPaginationRejectsMalformedCursorBeforeStore(t *testing.T) {
	st := newSocialFakeStore()
	st.identities[registryauth.HashToken("token")] = store.SessionIdentity{UserID: 7, Purpose: store.SessionCLI}
	handler := socialHandler(t, st, "")
	for _, path := range []string{
		"/v1/me/follows?limit=0", "/v1/me/follows?limit=51", "/v1/me/follows?cursor=%%%",
		"/v1/me/updates?cursor=" + encodeSocialCursor(map[string]any{"e": 0}),
		"/v1/me/updates?cursor=" + strings.Repeat("a", maxSocialCursorBytes+1),
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, socialRequest(http.MethodGet, path, "token", ""))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	if st.lastFollowPage != (store.FollowPage{}) || st.lastUpdatePage != (store.UpdatePage{}) {
		t.Fatalf("store called for invalid pagination: follow=%#v update=%#v", st.lastFollowPage, st.lastUpdatePage)
	}
}

func TestSocialRejectsInvalidRefBeforeMutation(t *testing.T) {
	st := newSocialFakeStore()
	st.identities[registryauth.HashToken("token")] = store.SessionIdentity{UserID: 7, Purpose: store.SessionCLI}
	rr := httptest.NewRecorder()
	socialHandler(t, st, "").ServeHTTP(rr, socialRequest(http.MethodPut, "/v1/me/follows/bad!/name", "token", ""))
	if rr.Code != http.StatusBadRequest || st.lastOwner != "" || st.lastName != "" {
		t.Fatalf("status=%d owner=%q name=%q", rr.Code, st.lastOwner, st.lastName)
	}
}

func TestSocialNotFoundMappings(t *testing.T) {
	st := newSocialFakeStore()
	st.identities[registryauth.HashToken("token")] = store.SessionIdentity{UserID: 7, Purpose: store.SessionCLI}
	st.followErr = store.ErrNotFound
	handler := socialHandler(t, st, "")
	for _, request := range []*http.Request{
		socialRequest(http.MethodPut, "/v1/me/follows/alice/missing", "token", ""),
		socialRequest(http.MethodPut, "/v1/me/follows/alice/missing/seen", "token", `{"version":1}`),
		socialRequest(http.MethodPut, "/v1/me/trials/alice/missing/1", "token", `{"verdict":"keep"}`),
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, request)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", request.URL.Path, rr.Code, rr.Body.String())
		}
	}
}

func TestFollowResponseUsesEmptyTagsAndRFC3339Times(t *testing.T) {
	view := followView(store.Follow{LatestPublishedAt: time.Date(2026, 7, 13, 12, 0, 0, 0, time.FixedZone("x", 3600))})
	if view.Tags == nil || view.LatestPublishedAt != "2026-07-13T11:00:00Z" {
		t.Fatalf("view = %#v", view)
	}
	if _, err := json.Marshal(view); err != nil {
		t.Fatal(err)
	}
}

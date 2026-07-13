package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistrySocialClientBuildsScopedRequests(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer user-session" {
			t.Errorf("authorization = %q", got)
		}
		if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
			t.Errorf("unexpected proxy headers: %v", r.Header)
		}
		switch requests.Load() {
		case 1:
			if r.Method != http.MethodPut || r.URL.EscapedPath() != "/prefix/v1/me/follows/alice/reviewer" {
				t.Errorf("follow request = %s %s", r.Method, r.URL.EscapedPath())
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprint(w, `{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","latest_version":3,"followed_at":"2026-07-13T12:00:00Z"}`)
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/prefix/v1/me/updates" || r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("cursor") != "a+b/c=" {
				t.Errorf("updates request = %s %s", r.Method, r.URL.String())
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"updates":[{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","version":3,"published_at":"2026-07-13T12:00:00Z"}]}`)
		case 3:
			if r.Method != http.MethodPut || r.URL.Path != "/prefix/v1/me/follows/alice/reviewer/seen" {
				t.Errorf("seen request = %s %s", r.Method, r.URL.Path)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type = %q", r.Header.Get("Content-Type"))
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ref":"@alice/reviewer","owner":"alice","name":"reviewer","last_seen_version":3,"followed_at":"2026-07-13T12:00:00Z"}`)
		case 4:
			if r.Method != http.MethodPut || r.URL.Path != "/prefix/v1/me/trials/alice/reviewer/3" {
				t.Errorf("trial request = %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		case 5:
			if r.Method != http.MethodDelete || r.URL.Path != "/prefix/v1/me/follows/alice/reviewer" {
				t.Errorf("unfollow request = %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		case 6:
			if r.Method != http.MethodDelete || r.URL.Path != "/prefix/v1/me/session" {
				t.Errorf("revoke request = %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	client, err := newRegistrySocialClient(strings.ToUpper("http")+strings.TrimPrefix(srv.URL, "http")+"/prefix/", "user-session")
	if err != nil {
		t.Fatal(err)
	}
	follow, err := client.Follow(context.Background(), "alice", "reviewer")
	if err != nil || follow.Ref != "@alice/reviewer" || follow.LatestVersion != 3 || follow.FollowedAt.IsZero() {
		t.Fatalf("Follow = %#v, %v", follow, err)
	}
	updates, err := client.ListUpdates(context.Background(), 25, "a+b/c=")
	if err != nil || len(updates.Updates) != 1 || updates.Updates[0].PublishedAt.IsZero() {
		t.Fatalf("ListUpdates = %#v, %v", updates, err)
	}
	if _, err := client.MarkSeen(context.Background(), "alice", "reviewer", 3); err != nil {
		t.Fatal(err)
	}
	if err := client.PutTrial(context.Background(), "alice", "reviewer", 3, "keep"); err != nil {
		t.Fatal(err)
	}
	if err := client.Unfollow(context.Background(), "alice", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := client.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrySocialClientClassifiesStaticErrors(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, errRegistryUnauthorized},
		{http.StatusForbidden, errRegistryForbidden},
		{http.StatusNotFound, errRegistryNotFound},
		{http.StatusGone, errRegistryGone},
		{http.StatusTooManyRequests, errRegistryRateLimited},
		{http.StatusBadGateway, errRegistryUnavailable},
		{http.StatusTeapot, errRegistryResponse},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"secret":"response-secret"}`)
			}))
			defer srv.Close()
			client, err := newRegistrySocialClient(srv.URL, "bearer-secret")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Follow(context.Background(), "alice", "reviewer")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			for _, secret := range []string{"bearer-secret", "response-secret", srv.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks %q: %v", secret, err)
				}
			}
		})
	}
}

func TestRegistrySocialClientRejectsRedirects(t *testing.T) {
	var sameTarget, crossTarget atomic.Int32
	cross := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		crossTarget.Add(1)
	}))
	defer cross.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "cross" {
			http.Redirect(w, r, cross.URL, http.StatusFound)
			return
		}
		if r.URL.Path == "/target" {
			sameTarget.Add(1)
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer srv.Close()
	client, err := newRegistrySocialClient(srv.URL, "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListUpdates(context.Background(), 1, "same"); !errors.Is(err, errRegistryUnavailable) {
		t.Fatalf("same-origin redirect error = %v", err)
	}
	if _, err := client.ListUpdates(context.Background(), 1, "cross"); !errors.Is(err, errRegistryUnavailable) {
		t.Fatalf("cross-origin redirect error = %v", err)
	}
	if sameTarget.Load() != 0 || crossTarget.Load() != 0 {
		t.Fatalf("redirect target requests = same %d, cross %d", sameTarget.Load(), crossTarget.Load())
	}
}

func TestRegistrySocialClientBoundsAndValidatesResponses(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"malformed", "application/json", "{"},
		{"wrong media type", "text/html", `{}`},
		{"oversized", "application/json", strings.Repeat("x", registrySocialBodyMax+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client, err := newRegistrySocialClient(srv.URL, "session")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListFollows(context.Background(), 1, ""); !errors.Is(err, errRegistryResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRegistrySocialClientHonorsContextAndRejectsInvalidLimits(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	client, err := newRegistrySocialClient(srv.URL, "session")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.ListUpdates(ctx, 1, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	before := requests.Load()
	for _, limit := range []int{0, -1, registrySocialListMax + 1} {
		if _, err := client.ListUpdates(context.Background(), limit, ""); err == nil {
			t.Fatalf("ListUpdates limit %d succeeded", limit)
		}
		if _, err := client.ListFollows(context.Background(), limit, ""); err == nil {
			t.Fatalf("ListFollows limit %d succeeded", limit)
		}
	}
	if requests.Load() != before {
		t.Fatalf("invalid limits made requests: before=%d after=%d", before, requests.Load())
	}
}

func TestNewRegistrySocialClientRequiresUserToken(t *testing.T) {
	if _, err := newRegistrySocialClient("https://registry.example", ""); !errors.Is(err, errRegistryUnauthorized) {
		t.Fatalf("error = %v", err)
	}
}

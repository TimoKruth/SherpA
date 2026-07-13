package auth

import (
	"context"
	"errors"
	"sync"
)

type PollResult struct {
	AccessToken string
	Err         error
}

type FakeGitHubClient struct {
	mu                sync.Mutex
	Device            DeviceCode
	StartErr          error
	PollResults       []PollResult
	Users             map[string]GitHubUser
	UserErr           error
	StartCalls        int
	PollCalls         int
	GetUserCalls      int
	AuthorizeURL      string
	AuthorizeErr      error
	WebToken          string
	WebTokenErr       error
	WebAuthorizeCalls int
	WebExchangeCalls  int
	LastWebState      string
	LastWebChallenge  string
	LastWebCallback   string
	LastWebCode       string
	LastWebVerifier   string
}

func (f *FakeGitHubClient) WebAuthorizeURL(state, challenge, callback string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WebAuthorizeCalls++
	f.LastWebState, f.LastWebChallenge, f.LastWebCallback = state, challenge, callback
	return f.AuthorizeURL, f.AuthorizeErr
}

func (f *FakeGitHubClient) ExchangeWebCode(_ context.Context, code, verifier, callback string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WebExchangeCalls++
	f.LastWebCode, f.LastWebVerifier, f.LastWebCallback = code, verifier, callback
	return f.WebToken, f.WebTokenErr
}

func (f *FakeGitHubClient) StartDeviceFlow(context.Context) (DeviceCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StartCalls++
	return f.Device, f.StartErr
}

func (f *FakeGitHubClient) PollToken(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PollCalls++
	if len(f.PollResults) == 0 {
		return "", ErrAuthPending
	}
	result := f.PollResults[0]
	f.PollResults = f.PollResults[1:]
	return result.AccessToken, result.Err
}

func (f *FakeGitHubClient) GetUser(_ context.Context, accessToken string) (GitHubUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetUserCalls++
	if f.UserErr != nil {
		return GitHubUser{}, f.UserErr
	}
	user, ok := f.Users[accessToken]
	if !ok {
		return GitHubUser{}, errors.New("fake GitHub user not scripted")
	}
	return user, nil
}

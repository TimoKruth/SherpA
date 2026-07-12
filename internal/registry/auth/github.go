package auth

import (
	"context"
	"errors"
)

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

type GitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type GitHubClient interface {
	StartDeviceFlow(ctx context.Context) (DeviceCode, error)
	PollToken(ctx context.Context, deviceCode string) (accessToken string, err error)
	GetUser(ctx context.Context, accessToken string) (GitHubUser, error)
}

var ErrAuthPending = errors.New("authorization pending")
var ErrSlowDown = errors.New("slow down")
var ErrExpired = errors.New("device code expired")

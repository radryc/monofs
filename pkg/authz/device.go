package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OAuthEndpoints holds the token and device-authorization endpoints of an IdP.
type OAuthEndpoints struct {
	DeviceAuthURL string
	TokenURL      string
	AuthURL       string // authorization endpoint (used by the PKCE web flow)
}

// TokenResponse is a normalized OAuth/OIDC token endpoint response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
	// ObtainedAt is set by the client when the token is received.
	ObtainedAt int64 `json:"obtained_at,omitempty"`
}

// IsExpired reports whether the access token has expired. Tokens without
// ExpiresIn are treated as non-expiring (e.g. opaque tokens verified by the
// server). Returns true only when ExpiresIn > 0 and the token has passed.
func (t *TokenResponse) IsExpired(now time.Time) bool {
	if t.ExpiresIn <= 0 || t.ObtainedAt <= 0 {
		return false
	}
	expiresAt := time.Unix(t.ObtainedAt, 0).Add(time.Duration(t.ExpiresIn) * time.Second)
	return !now.Before(expiresAt)
}

// CanRefresh reports whether the token has a refresh_token that may be used.
func (t *TokenResponse) CanRefresh() bool {
	return t.RefreshToken != ""
}

// MustRefreshSoon reports whether the token will expire within the given
// buffer (default 30s). Used to proactively refresh before expiry.
func (t *TokenResponse) MustRefreshSoon(now time.Time, buffer time.Duration) bool {
	if t.ExpiresIn <= 0 || t.ObtainedAt <= 0 {
		return false
	}
	expiresAt := time.Unix(t.ObtainedAt, 0).Add(time.Duration(t.ExpiresIn) * time.Second)
	return !now.Add(buffer).Before(expiresAt)
}

// DeviceAuthResponse is the device-authorization endpoint response.
type DeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceFlowClient runs the OAuth 2.0 Device Authorization Grant (RFC 8628),
// used for CLI login (guardianctl / monofs-admin) — authz epic F1.
type DeviceFlowClient struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	Endpoints    OAuthEndpoints
	HTTPClient   *http.Client
	// sleep and now are injectable for tests.
	sleep func(time.Duration)
	now   func() time.Time
}

// NewDeviceFlowClient builds a device-flow client with sane defaults.
func NewDeviceFlowClient(clientID string, scopes []string, endpoints OAuthEndpoints) *DeviceFlowClient {
	return &DeviceFlowClient{
		ClientID:   clientID,
		Scopes:     scopes,
		Endpoints:  endpoints,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		sleep:      time.Sleep,
		now:        time.Now,
	}
}

// Authorize requests a device + user code from the IdP.
func (c *DeviceFlowClient) Authorize(ctx context.Context) (*DeviceAuthResponse, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	if len(c.Scopes) > 0 {
		form.Set("scope", strings.Join(c.Scopes, " "))
	}
	resp, err := c.postForm(ctx, c.Endpoints.DeviceAuthURL, form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authz: device authorization failed with status %d", resp.StatusCode)
	}
	var out DeviceAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("authz: decode device authorization: %w", err)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return &out, nil
}

// Poll polls the token endpoint until the user approves, the flow is denied, or
// the device code expires.
func (c *DeviceFlowClient) Poll(ctx context.Context, dev *DeviceAuthResponse) (*TokenResponse, error) {
	interval := time.Duration(dev.Interval) * time.Second
	deadline := c.now().Add(time.Duration(dev.ExpiresIn) * time.Second)

	for {
		if !deadline.IsZero() && c.now().After(deadline) {
			return nil, fmt.Errorf("authz: device flow expired before authorization")
		}

		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", dev.DeviceCode)
		form.Set("client_id", c.ClientID)
		if c.ClientSecret != "" {
			form.Set("client_secret", c.ClientSecret)
		}

		resp, err := c.postForm(ctx, c.Endpoints.TokenURL, form)
		if err != nil {
			return nil, err
		}
		body := struct {
			TokenResponse
			Error string `json:"error"`
		}{}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK && body.AccessToken != "" {
			tok := body.TokenResponse
			tok.ObtainedAt = c.now().Unix()
			return &tok, nil
		}

		switch body.Error {
		case "authorization_pending":
			// keep waiting
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, fmt.Errorf("authz: device flow denied by user")
		case "expired_token":
			return nil, fmt.Errorf("authz: device code expired")
		default:
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("authz: device token poll failed (status %d, error %q)", resp.StatusCode, body.Error)
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		c.sleep(interval)
	}
}

func (c *DeviceFlowClient) postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("authz: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return c.HTTPClient.Do(req)
}

// SaveTokenFile persists a token to disk with 0600 permissions.
func SaveTokenFile(path string, tok *TokenResponse) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("authz: token path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("authz: create token dir: %w", err)
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("authz: encode token: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("authz: write token: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadTokenFile reads a token previously saved with SaveTokenFile.
func LoadTokenFile(path string) (*TokenResponse, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok TokenResponse
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("authz: decode token: %w", err)
	}
	return &tok, nil
}

// RefreshTokenConfig holds the parameters needed to exchange a refresh_token
// for a new access token.
type RefreshTokenConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	HTTPClient   *http.Client
}

// RefreshToken exchanges the refresh_token for a new access/id_token pair.
func RefreshToken(ctx context.Context, cfg RefreshTokenConfig, refreshToken string) (*TokenResponse, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", cfg.ClientID)
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("authz: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authz: refresh request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authz: refresh failed with status %d", resp.StatusCode)
	}
	var tok TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("authz: decode refresh token: %w", err)
	}
	tok.ObtainedAt = time.Now().Unix()
	return &tok, nil
}

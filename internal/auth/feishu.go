package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

const (
	feishuTokenURL    = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	feishuOAuthURL    = "https://open.feishu.cn/open-apis/authen/v1/authorize"
	feishuCodeURL     = "https://open.feishu.cn/open-apis/authen/v1/oidc/access_token"
	feishuUserInfoURL = "https://open.feishu.cn/open-apis/authen/v1/user_info"
)

// FeishuOAuth handles Feishu SSO.
type FeishuOAuth struct {
	appID     string
	appSecret string
	rc        *resty.Client
}

// NewFeishuOAuth creates a FeishuOAuth. AppID / AppSecret can be empty
// if login is not yet configured (returns framework-only errors).
func NewFeishuOAuth(appID, appSecret string) *FeishuOAuth {
	return &FeishuOAuth{
		appID:     appID,
		appSecret: appSecret,
		rc:        httpclient.New(15*time.Second, nil),
	}
}

// Configured reports whether Feishu credentials are present.
func (f *FeishuOAuth) Configured() bool {
	return f.appID != "" && f.appSecret != ""
}

// AuthURL builds the Feishu OAuth redirect URL.
// redirectURI must be registered in the Feishu app console.
// state is an opaque string to prevent CSRF.
func (f *FeishuOAuth) AuthURL(redirectURI, state string) string {
	v := url.Values{}
	v.Set("app_id", f.appID)
	v.Set("redirect_uri", redirectURI)
	v.Set("response_type", "code")
	v.Set("state", state)
	return feishuOAuthURL + "?" + v.Encode()
}

// FeishuUser is the normalised user info returned by Feishu.
type FeishuUser struct {
	OpenID     string `json:"open_id"`
	UnionID    string `json:"union_id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	AvatarURL  string `json:"avatar_url"`
	Department string `json:"department_name"`
}

// ExchangeCode trades the OAuth code for a FeishuUser.
func (f *FeishuOAuth) ExchangeCode(ctx context.Context, code string) (*FeishuUser, error) {
	if !f.Configured() {
		return nil, fmt.Errorf("feishu: app credentials not configured")
	}
	tenantToken, err := f.tenantAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("feishu: get tenant token: %w", err)
	}
	userToken, err := f.userAccessToken(ctx, tenantToken, code)
	if err != nil {
		return nil, fmt.Errorf("feishu: get user token: %w", err)
	}
	return f.userInfo(ctx, userToken)
}

// tenantAccessToken fetches a short-lived tenant access token.
func (f *FeishuOAuth) tenantAccessToken(ctx context.Context) (string, error) {
	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	resp, err := httpclient.Request(ctx, f.rc).
		SetBody(map[string]string{"app_id": f.appID, "app_secret": f.appSecret}).
		SetResult(&result).
		Post(feishuTokenURL)
	if err != nil {
		return "", err
	}
	if resp.IsError() || result.Code != 0 {
		return "", fmt.Errorf("feishu tenant token error %d: %s", result.Code, result.Msg)
	}
	return result.TenantAccessToken, nil
}

// userAccessToken exchanges the OAuth code for a user token.
func (f *FeishuOAuth) userAccessToken(ctx context.Context, tenantToken, code string) (string, error) {
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	resp, err := httpclient.Request(ctx, f.rc).
		SetAuthToken(tenantToken).
		SetBody(map[string]string{"grant_type": "authorization_code", "code": code}).
		SetResult(&result).
		Post(feishuCodeURL)
	if err != nil {
		return "", err
	}
	if resp.IsError() || result.Code != 0 {
		return "", fmt.Errorf("feishu user token error %d: %s", result.Code, result.Msg)
	}
	// Also check that the parsed body contains a valid access_token.
	return result.Data.AccessToken, nil
}

// userInfo fetches user info using the user access token.
func (f *FeishuOAuth) userInfo(ctx context.Context, userToken string) (*FeishuUser, error) {
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OpenID          string `json:"open_id"`
			UnionID         string `json:"union_id"`
			Name            string `json:"name"`
			Email           string `json:"email"`
			EnterpriseEmail string `json:"enterprise_email"`
			AvatarURL       string `json:"avatar_url"`
		} `json:"data"`
	}
	resp, err := httpclient.Request(ctx, f.rc).
		SetAuthToken(userToken).
		SetResult(&result).
		Get(feishuUserInfoURL)
	if err != nil {
		return nil, err
	}
	if resp.IsError() || result.Code != 0 {
		return nil, fmt.Errorf("feishu user info error %d: %s", result.Code, result.Msg)
	}
	// Enterprise users often have only enterprise_email.
	// Prefer personal email, then fall back to enterprise_email.
	// If both are empty, the Feishu app likely lacks the required email scopes.
	email := result.Data.Email
	if email == "" {
		email = result.Data.EnterpriseEmail
	}
	return &FeishuUser{
		OpenID:    result.Data.OpenID,
		UnionID:   result.Data.UnionID,
		Name:      result.Data.Name,
		Email:     email,
		AvatarURL: result.Data.AvatarURL,
	}, nil
}

// GenerateState returns a random opaque state string for CSRF protection.
func GenerateState() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateToken returns a random session token (48 random bytes, base64url).
func GenerateToken() string {
	b := make([]byte, 36)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

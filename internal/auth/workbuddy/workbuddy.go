// Package workbuddy implements WorkBuddy's site-scoped browser authorization.
package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	ProviderCN       = "workbuddy"
	ProviderAI       = "workbuddy-ai"
	AppVersion       = "2.0.0"
	UserAgentVersion = "5.5.6"
	LoginTimeout     = 5 * time.Minute
	RefreshLead      = 5 * time.Minute
)

// Site keeps authorization, refresh and inference on the same regional service.
type Site struct {
	Provider string
	Name     string
	Endpoint string
}

func SiteForProvider(provider string) Site {
	if provider == ProviderAI {
		return Site{ProviderAI, "WorkBuddy AI", "https://www.workbuddy.ai"}
	}
	return Site{ProviderCN, "WorkBuddy", "https://copilot.tencent.com"}
}

// Credentials includes the identity required by WorkBuddy's inference gateway.
type Credentials struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
	TokenType        string `json:"tokenType"`
	UID              string `json:"uid"`
	EnterpriseID     string `json:"enterpriseId"`
	Nickname         string `json:"nickname"`
	Email            string `json:"email"`
}

type LoginSession struct {
	State string
	URL   string
}

type APIError struct {
	Code   int
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("workbuddy: %s (HTTP %d, code %d)", e.Msg, e.Status, e.Code)
}

func (e *APIError) StatusCode() int {
	if e.Status >= 400 {
		return e.Status
	}
	return http.StatusBadGateway
}

type Service struct {
	site         Site
	client       *http.Client
	pollInterval time.Duration
}

func NewService(provider string, client *http.Client) *Service {
	return &Service{site: SiteForProvider(provider), client: client, pollInterval: time.Second}
}

// SetEndpoint supports account-level upstream configuration and local verification.
func (s *Service) SetEndpoint(endpoint string) {
	s.site.Endpoint = strings.TrimRight(endpoint, "/")
}

func (s *Service) call(ctx context.Context, method, path string, headers http.Header, dst any) error {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.site.Endpoint+path, body)
	if err != nil {
		return err
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "WorkBuddy/"+UserAgentVersion)
	res, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("workbuddy: request failed: %w", err)
	}
	defer func() {
		if errClose := res.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("workbuddy: close authorization response")
		}
	}()
	var envelope struct {
		Code json.Number     `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	errDecode := json.NewDecoder(res.Body).Decode(&envelope)
	code, _ := envelope.Code.Int64()
	if res.StatusCode < 200 || res.StatusCode >= 300 || code != 0 {
		message := envelope.Msg
		if message == "" {
			message = http.StatusText(res.StatusCode)
		}
		return &APIError{int(code), res.StatusCode, message}
	}
	if errDecode != nil {
		return fmt.Errorf("workbuddy: invalid response: %w", errDecode)
	}
	if dst == nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) || bytes.Equal(envelope.Data, []byte("{}")) {
		return nil
	}
	return json.Unmarshal(envelope.Data, dst)
}

func (s *Service) Start(ctx context.Context) (*LoginSession, error) {
	var result struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	headers := make(http.Header)
	for _, key := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		headers.Set(key, "true")
	}
	if err := s.call(ctx, http.MethodPost, "/v2/plugin/auth/state?platform="+url.QueryEscape(s.site.Provider), headers, &result); err != nil {
		return nil, err
	}
	authURL, err := url.Parse(result.AuthURL)
	if err != nil || authURL.Scheme != "https" || authURL.Host == "" || result.State == "" {
		return nil, fmt.Errorf("workbuddy: invalid browser authorization session")
	}
	query := authURL.Query()
	query.Set("version", AppVersion)
	query.Set("loginSessionId", strings.ReplaceAll(uuid.NewString(), "-", ""))
	authURL.RawQuery = query.Encode()
	return &LoginSession{State: result.State, URL: authURL.String()}, nil
}

func (s *Service) poll(ctx context.Context, path string, headers http.Header, pendingCode int, dst any, ready func() bool) error {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.call(ctx, http.MethodGet, path, headers, dst)
		if err == nil && ready() {
			return nil
		}
		if err != nil {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || (apiErr.Code != pendingCode && apiErr.Status != http.StatusRequestTimeout && apiErr.Status != http.StatusTooManyRequests) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) Wait(ctx context.Context, session *LoginSession) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, LoginTimeout)
	defer cancel()
	state := url.QueryEscape(session.State)
	var token Credentials
	if err := s.poll(ctx, "/v2/plugin/auth/token?state="+state, nil, 11217, &token, func() bool { return token.AccessToken != "" }); err != nil {
		return Credentials{}, err
	}
	token = mergeTokens(Credentials{}, token, time.Now())
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token.AccessToken)
	headers.Set("X-Domain", s.domain(token))
	headers.Set("X-No-User-Id", "true")
	headers.Set("X-No-Enterprise-Id", "true")
	var who struct {
		UID, Nickname, Email, PhoneNumber, Type, EnterpriseID string
	}
	if err := s.poll(ctx, "/v2/plugin/login/account?state="+state, headers, 12151, &who, func() bool { return who.UID != "" }); err != nil {
		return Credentials{}, err
	}
	token.UID, token.EnterpriseID, token.Nickname, token.Email = who.UID, strings.TrimSpace(who.EnterpriseID), who.Nickname, who.Email
	if token.Nickname == "" {
		token.Nickname = who.PhoneNumber
	}
	if token.EnterpriseID == "" && who.Type != "" && !strings.EqualFold(who.Type, "personal") {
		var list struct {
			Accounts []struct {
				UID, Type, EnterpriseID string
				LastLogin               bool
			} `json:"accounts"`
		}
		if err := s.call(ctx, http.MethodGet, "/v2/plugin/accounts", headers, &list); err != nil {
			return Credentials{}, fmt.Errorf("workbuddy: resolve enterprise account: %w", err)
		}
		var matching, recent, all []string
		for _, account := range list.Accounts {
			if account.UID != who.UID || strings.TrimSpace(account.EnterpriseID) == "" {
				continue
			}
			all = append(all, account.EnterpriseID)
			if strings.EqualFold(account.Type, who.Type) {
				matching = append(matching, account.EnterpriseID)
			}
			if account.LastLogin {
				recent = append(recent, account.EnterpriseID)
			}
		}
		for _, candidates := range [][]string{matching, recent, all} {
			if len(candidates) == 1 {
				token.EnterpriseID = candidates[0]
				break
			}
		}
		if token.EnterpriseID == "" {
			return Credentials{}, fmt.Errorf("workbuddy: cannot determine enterprise identity")
		}
	}
	return token, nil
}

func (s *Service) domain(creds Credentials) string {
	if creds.Domain != "" {
		return creds.Domain
	}
	endpoint, _ := url.Parse(s.site.Endpoint)
	return endpoint.Host
}

func (s *Service) Refresh(ctx context.Context, creds Credentials) (Credentials, error) {
	if creds.RefreshToken == "" || (creds.RefreshExpiresAt > 0 && time.Now().UnixMilli() >= creds.RefreshExpiresAt) {
		return Credentials{}, fmt.Errorf("workbuddy: refresh token expired or missing; sign in again")
	}
	headers := make(http.Header)
	headers.Set("X-Refresh-Token", creds.RefreshToken)
	headers.Set("X-Auth-Refresh-Source", "plugin")
	headers.Set("X-Domain", s.domain(creds))
	if creds.EnterpriseID != "" {
		headers.Set("X-Enterprise-Id", creds.EnterpriseID)
		headers.Set("X-Tenant-Id", creds.EnterpriseID)
	}
	var next Credentials
	if err := s.call(ctx, http.MethodPost, "/v2/plugin/auth/token/refresh", headers, &next); err != nil {
		return Credentials{}, err
	}
	if next.AccessToken == "" {
		return Credentials{}, fmt.Errorf("workbuddy: refresh returned no access token")
	}
	return mergeTokens(creds, next, time.Now()), nil
}

type Quota struct {
	Plan    string       `json:"plan"`
	Credits *CreditQuota `json:"credits"`
}

type CreditQuota struct {
	Total            float64 `json:"total"`
	Used             float64 `json:"used"`
	Remaining        float64 `json:"remaining"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
}

func quotaCapacity(value json.RawMessage) (float64, error) {
	if len(value) == 0 || bytes.Equal(value, []byte(`""`)) {
		return 0, nil
	}
	var number json.Number
	if err := json.Unmarshal(value, &number); err != nil {
		return 0, err
	}
	if number == "" {
		return 0, nil
	}
	return strconv.ParseFloat(string(number), 64)
}

func creditQuota(total, used float64) *CreditQuota {
	if total <= 0 {
		return nil
	}
	remaining := max(0, total-used)
	return &CreditQuota{Total: total, Used: used, Remaining: remaining, RemainingPercent: min(100, remaining/total*100)}
}

// FetchQuota reads billing credits without changing account routing or cooldowns.
func (s *Service) FetchQuota(ctx context.Context, creds Credentials, plan string) (Quota, error) {
	signed := &http.Request{Header: make(http.Header)}
	SignRequest(signed, s.site.Provider, creds)
	var summary struct {
		IsPaidUser bool
		Packages   []struct{ CycleTotalCapacity, CycleUsedCapacity json.RawMessage }
	}
	if err := s.call(ctx, http.MethodPost, "/billing/meter/get-user-resource-summary", signed.Header, &summary); err != nil {
		return Quota{}, err
	}
	quota := Quota{Plan: plan}
	if quota.Plan == "" {
		quota.Plan = "Free"
		if summary.IsPaidUser {
			quota.Plan = "Pro"
		}
	}
	var total, used float64
	for _, pack := range summary.Packages {
		capacity, err := quotaCapacity(pack.CycleTotalCapacity)
		if err != nil {
			return Quota{}, fmt.Errorf("workbuddy: invalid credit capacity: %w", err)
		}
		consumed, err := quotaCapacity(pack.CycleUsedCapacity)
		if err != nil {
			return Quota{}, fmt.Errorf("workbuddy: invalid credit usage: %w", err)
		}
		total += capacity
		used += consumed
	}
	quota.Credits = creditQuota(total, used)
	if quota.Credits != nil || creds.EnterpriseID == "" {
		return quota, nil
	}
	var enterprise struct {
		LimitNum, Credit json.RawMessage
		CycleResetTime   string
	}
	if err := s.call(ctx, http.MethodPost, "/billing/meter/get-enterprise-user-usage", signed.Header, &enterprise); err != nil {
		return Quota{}, err
	}
	total, err := quotaCapacity(enterprise.LimitNum)
	if err != nil {
		return Quota{}, fmt.Errorf("workbuddy: invalid enterprise credit capacity: %w", err)
	}
	used, err = quotaCapacity(enterprise.Credit)
	if err != nil {
		return Quota{}, fmt.Errorf("workbuddy: invalid enterprise credit usage: %w", err)
	}
	quota.Credits = creditQuota(total, used)
	if plan == "" {
		quota.Plan = "Enterprise"
	}
	if quota.Credits != nil && enterprise.CycleResetTime != "" {
		reset, errParse := time.ParseInLocation("2006-01-02 15:04:05", enterprise.CycleResetTime, time.FixedZone("UTC+8", 8*60*60))
		if errParse == nil {
			quota.Credits.ResetsAt = reset.UTC().Format(time.RFC3339)
		}
	}
	return quota, nil
}

func mergeTokens(creds, next Credentials, now time.Time) Credentials {
	creds.AccessToken = next.AccessToken
	if next.RefreshToken != "" {
		creds.RefreshToken = next.RefreshToken
	}
	if next.Domain != "" {
		creds.Domain = next.Domain
	}
	if next.TokenType != "" {
		creds.TokenType = next.TokenType
	}
	if next.ExpiresAt > 0 {
		creds.ExpiresAt = next.ExpiresAt
	} else if next.ExpiresIn > 0 {
		creds.ExpiresAt = now.UnixMilli() + next.ExpiresIn*1000
	}
	if next.RefreshExpiresAt > 0 {
		creds.RefreshExpiresAt = next.RefreshExpiresAt
	} else if next.RefreshExpiresIn > 0 {
		creds.RefreshExpiresAt = now.UnixMilli() + next.RefreshExpiresIn*1000
	}
	return creds
}

func NewAuthRecord(provider string, creds Credentials) *coreauth.Auth {
	identity := sha256.Sum256([]byte(creds.UID + "\n" + creds.EnterpriseID))
	fileName := fmt.Sprintf("%s-%x.json", provider, identity[:12])
	label := creds.Nickname
	if label == "" {
		label = creds.UID
	}
	record := &coreauth.Auth{ID: fileName, FileName: fileName, Provider: provider, Label: label, Status: coreauth.StatusActive, Metadata: map[string]any{}}
	ApplyCredentials(record, creds)
	return record
}

func ApplyCredentials(record *coreauth.Auth, creds Credentials) {
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	for key, value := range map[string]any{
		"type": record.Provider, "auth_kind": "oauth", "access_token": creds.AccessToken, "refresh_token": creds.RefreshToken,
		"uid": creds.UID, "enterprise_id": creds.EnterpriseID, "domain": creds.Domain, "token_type": creds.TokenType,
		"nickname": creds.Nickname, "label": record.Label, "email": creds.Email, "expires_at": creds.ExpiresAt, "refresh_expires_at": creds.RefreshExpiresAt,
	} {
		record.Metadata[key] = value
	}
	if creds.ExpiresAt > 0 {
		record.Metadata["expired"] = time.UnixMilli(creds.ExpiresAt).UTC().Format(time.RFC3339)
	}
}

func CredentialsFromAuth(record *coreauth.Auth) Credentials {
	data, _ := json.Marshal(record.Metadata)
	var stored struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        int64  `json:"expires_at"`
		RefreshExpiresAt int64  `json:"refresh_expires_at"`
		Domain           string `json:"domain"`
		TokenType        string `json:"token_type"`
		UID              string `json:"uid"`
		EnterpriseID     string `json:"enterprise_id"`
		Nickname         string `json:"nickname"`
		Email            string `json:"email"`
	}
	_ = json.Unmarshal(data, &stored)
	return Credentials{AccessToken: stored.AccessToken, RefreshToken: stored.RefreshToken, ExpiresAt: stored.ExpiresAt,
		RefreshExpiresAt: stored.RefreshExpiresAt, Domain: stored.Domain, TokenType: stored.TokenType,
		UID: stored.UID, EnterpriseID: stored.EnterpriseID, Nickname: stored.Nickname, Email: stored.Email}
}

func EndpointForAuth(record *coreauth.Auth) string {
	if endpoint, _ := record.Metadata["base_url"].(string); strings.TrimSpace(endpoint) != "" {
		return strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v2")
	}
	return SiteForProvider(record.Provider).Endpoint
}

// SignRequest applies the desktop protocol's account and product headers.
func SignRequest(req *http.Request, provider string, creds Credentials) {
	domain := creds.Domain
	if domain == "" {
		endpoint, _ := url.Parse(SiteForProvider(provider).Endpoint)
		domain = endpoint.Host
	}
	for name, value := range map[string]string{
		"Authorization": "Bearer " + creds.AccessToken, "X-User-Id": creds.UID, "X-Domain": domain,
		"X-Product": "SaaS", "X-IDE-Type": "WorkBuddy", "User-Agent": "WorkBuddy/" + UserAgentVersion,
		"X-Requested-With": "XMLHttpRequest", "X-Agent-Intent": "craft", "X-Agent-Purpose": "conversation",
		"X-IDE-Name": "WorkBuddy", "X-IDE-Version": UserAgentVersion,
	} {
		req.Header.Set(name, value)
	}
	if creds.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", creds.EnterpriseID)
		req.Header.Set("X-Tenant-Id", creds.EnterpriseID)
	}
	if provider == ProviderAI {
		req.Header.Set("X-Agent-Type", "main")
	}
}

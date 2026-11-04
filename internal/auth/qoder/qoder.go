// Package qoder implements Qoder's regional device authorization and account APIs.
package qoder

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	ProviderIntl     = "qoder"
	ProviderCN       = "qoder-cn"
	LoginTimeout     = 10 * time.Minute
	RefreshLead      = 4 * time.Minute
	UserAgentVersion = "1.1.75"
)

type Site struct {
	Provider string
	Realm    string
	Name     string
	Website  string
	OpenAPI  string
	Gateway  string
	ClientID string
}

func SiteForProvider(provider string) Site {
	if provider == ProviderCN {
		return Site{ProviderCN, "cn", "Qoder CN", "https://qoder.com.cn", "https://openapi.qoder.com.cn", "https://gateway.qoder.com.cn", "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"}
	}
	return Site{ProviderIntl, "intl", "Qoder", "https://qoder.com", "https://openapi.qoder.sh", "https://api3.qoder.sh", "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"}
}

func (s Site) userAgent() string {
	if s.Provider == ProviderCN {
		return "QoderWork/" + UserAgentVersion
	}
	return "Qoder/" + UserAgentVersion
}

// ExpiresAt is Unix seconds; the upstream expires_in field is milliseconds.
type Credentials struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	PersonalToken    string `json:"personal_token"`
	UID              string `json:"uid"`
	Nickname         string `json:"nickname"`
	Email            string `json:"email"`
	UserType         string `json:"user_type"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	Realm            string `json:"realm"`
	Domain           string `json:"domain"`
	MachineID        string `json:"machine_id"`
	ExpiresAt        int64  `json:"expires_at"`
}

type LoginSession struct {
	URL       string
	State     string
	Verifier  string
	Nonce     string
	MachineID string
	createdAt time.Time
	provider  string
}

// APIError deliberately excludes upstream bodies, URLs and credentials.
type APIError struct {
	Status int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("qoder: upstream request failed (HTTP %d)", e.Status)
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
	nowFunc      func() time.Time
}

func NewService(provider string, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{site: SiteForProvider(provider), client: client, pollInterval: time.Second, nowFunc: time.Now}
}

// SetEndpoint overrides only OpenAPI, retaining the regional browser and gateway URLs.
func (s *Service) SetEndpoint(endpoint string) {
	if endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/"); endpoint != "" {
		s.site.OpenAPI = endpoint
	}
}

func (s *Service) Start(ctx context.Context) (*LoginSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("qoder: generate authorization verifier")
	}
	for i, b := range raw {
		raw[i] = alphabet[int(b)%len(alphabet)]
	}
	session := &LoginSession{
		State: "qd-" + uuid.NewString(), Verifier: string(raw), Nonce: uuid.NewString(),
		MachineID: uuid.NewString(), createdAt: s.nowFunc(), provider: s.site.Provider,
	}
	if s.site.Provider == ProviderIntl {
		session.Nonce = strings.ReplaceAll(session.Nonce, "-", "")
	}
	challenge := sha256.Sum256(raw)
	query := url.Values{
		"challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "challenge_method": {"S256"},
		"nonce": {session.Nonce}, "client_id": {s.site.ClientID}, "machine_id": {session.MachineID},
	}
	if s.site.Provider == ProviderCN {
		query.Set("redirect_uri", "qoder-work-cn://")
	}
	session.URL = s.site.Website + "/device/selectAccounts?" + query.Encode()
	return session, nil
}

func (s *Service) Wait(ctx context.Context, session *LoginSession) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	if session == nil || session.State == "" || session.Nonce == "" || session.Verifier == "" || session.MachineID == "" ||
		(session.provider != "" && session.provider != s.site.Provider) {
		return Credentials{}, fmt.Errorf("qoder: invalid authorization session")
	}
	remaining := LoginTimeout
	if !session.createdAt.IsZero() {
		remaining -= s.nowFunc().Sub(session.createdAt)
	}
	if remaining <= 0 {
		return Credentials{}, fmt.Errorf("qoder: authorization session expired")
	}
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	query := url.Values{"nonce": {session.Nonce}, "verifier": {session.Verifier}, "challenge_method": {"S256"}}
	headers := http.Header{"Accept": {"application/json"}, "User-Agent": {"QoderWork"}}
	for {
		data, status, err := s.call(ctx, http.MethodGet, "/api/v1/deviceToken/poll?"+query.Encode(), nil, headers, true)
		if err != nil {
			return Credentials{}, err
		}
		if status == http.StatusOK && firstString(data, "token", "device_token") != "" {
			creds, errToken := s.mergeToken(Credentials{MachineID: session.MachineID}, data, true)
			if errToken != nil {
				return Credentials{}, errToken
			}
			creds.UID = identity(data["user_id"])
			return s.resolveIdentity(ctx, creds)
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Credentials{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) resolveIdentity(ctx context.Context, creds Credentials) (Credentials, error) {
	data, _, err := s.call(ctx, http.MethodGet, "/api/v1/userinfo", nil, apiHeaders(s.site.Provider, creds), false)
	if errContext := ctx.Err(); errContext != nil {
		return Credentials{}, errContext
	}
	if err == nil {
		uid := identity(data["id"])
		if creds.UID != "" && uid != "" && creds.UID != uid {
			return Credentials{}, fmt.Errorf("qoder: inconsistent account identity")
		}
		if uid != "" {
			creds.UID = uid
		}
		creds.Nickname = firstString(data, "name")
		creds.Email = firstString(data, "email")
		creds.UserType = firstString(data, "user_type")
		creds.OrganizationID = identity(data["organization_id"])
		if creds.OrganizationID == "" {
			creds.OrganizationID = identity(data["organizationId"])
		}
		creds.OrganizationName = firstString(data, "organization_name", "organizationName")
	}
	if creds.UID == "" {
		return Credentials{}, fmt.Errorf("qoder: authorization returned no authoritative user identity")
	}
	if creds.UserType == "" {
		creds.UserType = "personal_professional_trial"
	}
	return normalizeCredentials(s.site.Provider, creds), nil
}

// Refresh preserves identity and only falls back to PAT for the job-token family.
func (s *Service) Refresh(ctx context.Context, creds Credentials) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	if creds.Realm != "" && creds.Realm != s.site.Realm {
		return Credentials{}, fmt.Errorf("qoder: credential region does not match provider")
	}
	if strings.HasPrefix(creds.RefreshToken, "drt-") {
		return s.postToken(ctx, "/api/v1/deviceToken/refresh", map[string]string{"refresh_token": creds.RefreshToken}, creds, true)
	}
	var errRefresh error
	if creds.RefreshToken != "" {
		var next Credentials
		next, errRefresh = s.postToken(ctx, "/api/v1/jobToken/refresh", map[string]string{"refresh_token": creds.RefreshToken}, creds, false)
		if errRefresh == nil {
			return next, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	if creds.PersonalToken != "" {
		return s.postToken(ctx, "/api/v1/jobToken/exchange", map[string]string{"personal_token": creds.PersonalToken}, creds, false)
	}
	if errRefresh != nil {
		return Credentials{}, errRefresh
	}
	return Credentials{}, fmt.Errorf("qoder: no refresh token or personal token; sign in again")
}

func (s *Service) postToken(ctx context.Context, path string, payload map[string]string, creds Credentials, device bool) (Credentials, error) {
	data, _, err := s.call(ctx, http.MethodPost, path, payload, nil, false)
	if err != nil {
		return Credentials{}, err
	}
	next, errToken := s.mergeToken(creds, data, device)
	if errToken != nil {
		return Credentials{}, errToken
	}
	uid := identity(data["user_id"])
	if creds.UID != "" && uid != "" && creds.UID != uid {
		return Credentials{}, fmt.Errorf("qoder: inconsistent account identity")
	}
	if next.UID == "" {
		next.UID = uid
		return s.resolveIdentity(ctx, next)
	}
	return next, nil
}

func (s *Service) mergeToken(creds Credentials, data map[string]any, device bool) (Credentials, error) {
	token := firstString(data, "token")
	if token == "" && device {
		token = firstString(data, "device_token")
	}
	if token == "" {
		return Credentials{}, fmt.Errorf("qoder: token response contains no access token")
	}
	expiry, err := tokenExpiry(data, creds.ExpiresAt, s.nowFunc())
	if err != nil {
		return Credentials{}, err
	}
	creds.AccessToken, creds.ExpiresAt = token, expiry
	if refresh := firstString(data, "refresh_token"); refresh != "" {
		creds.RefreshToken = refresh
	}
	return normalizeCredentials(s.site.Provider, creds), nil
}

func tokenExpiry(data map[string]any, previous int64, now time.Time) (int64, error) {
	if value := data["expires_in"]; value != nil && value != "" {
		ms, ok := finiteNumber(value)
		if !ok || ms < 0 || ms > float64(253402300799-now.Unix())*1000 {
			return 0, fmt.Errorf("qoder: invalid token expiry")
		}
		if ms > 0 {
			return now.Unix() + int64((float64(now.Nanosecond())/1e6+ms)/1000), nil
		}
	}
	if value := data["expires_at"]; value != nil && value != "" {
		if expiry, ok := absoluteExpiry(value); ok {
			return expiry, nil
		}
		return 0, fmt.Errorf("qoder: invalid token expiry")
	}
	// An omitted lifetime must not extend or erase a previously known expiry.
	return previous, nil
}

func (s *Service) call(ctx context.Context, method, path string, payload any, headers http.Header, polling bool) (map[string]any, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("qoder: invalid request payload")
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.site.OpenAPI+path, body)
	if err != nil {
		return nil, 0, fmt.Errorf("qoder: invalid OpenAPI endpoint")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", s.site.userAgent())
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	res, errRequest := s.client.Do(req)
	if errRequest != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, 0, errContext
		}
		// Transport errors can include the verifier query or a credential-bearing URL.
		return nil, 0, fmt.Errorf("qoder: upstream request failed")
	}
	defer func() {
		if errClose := res.Body.Close(); errClose != nil {
			log.Debug("qoder: close upstream response failed")
		}
	}()
	if polling && (res.StatusCode == http.StatusAccepted || res.StatusCode == http.StatusNotFound) {
		return nil, res.StatusCode, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode, &APIError{Status: res.StatusCode}
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, 2<<20))
	decoder.UseNumber()
	var data map[string]any
	if errDecode := decoder.Decode(&data); errDecode != nil || data == nil {
		return nil, res.StatusCode, fmt.Errorf("qoder: invalid upstream response")
	}
	if errDecode := decoder.Decode(new(any)); errDecode != io.EOF {
		return nil, res.StatusCode, fmt.Errorf("qoder: invalid upstream response")
	}
	if data["success"] == false || data["error"] != nil && data["error"] != "" {
		return nil, res.StatusCode, fmt.Errorf("qoder: upstream rejected request")
	}
	if code, exists := data["code"]; exists && code != nil {
		if number, ok := finiteNumber(code); !ok || number != 0 {
			return nil, res.StatusCode, fmt.Errorf("qoder: upstream rejected request")
		}
	}
	return data, res.StatusCode, nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func identity(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return v.String()
		}
	}
	return ""
}

func finiteNumber(value any) (float64, bool) {
	var text string
	switch v := value.(type) {
	case json.Number:
		text = v.String()
	case string:
		text = strings.TrimSpace(v)
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
	n, err := strconv.ParseFloat(text, 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func absoluteExpiry(value any) (int64, bool) {
	if text, ok := value.(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err == nil && parsed.Unix() > 0 {
			return parsed.Unix(), true
		}
	}
	n, ok := finiteNumber(value)
	if n > 1e12 {
		n /= 1000
	}
	if !ok || n <= 0 || n > 253402300799 {
		return 0, false
	}
	return int64(n), true
}

func normalizeCredentials(provider string, creds Credentials) Credentials {
	site := SiteForProvider(provider)
	if creds.Realm == "" {
		creds.Realm = site.Realm
	}
	if creds.Domain == "" {
		creds.Domain = strings.TrimPrefix(site.Website, "https://")
	}
	if creds.MachineID == "" && creds.UID != "" {
		creds.MachineID = DeriveID(creds.UID, "machine")
	}
	return creds
}

func NewAuthRecord(provider string, creds Credentials) *coreauth.Auth {
	if strings.TrimSpace(creds.UID) == "" {
		return nil
	}
	provider = SiteForProvider(provider).Provider
	creds = normalizeCredentials(provider, creds)
	encoded, _ := json.Marshal([]string{provider, creds.Realm, creds.UID, creds.OrganizationID})
	digest := sha256.Sum256(encoded)
	fileName := fmt.Sprintf("%s-%x.json", provider, digest[:12])
	label := creds.Nickname
	if label == "" {
		label = creds.UID
	}
	record := &coreauth.Auth{ID: fileName, FileName: fileName, Provider: provider, Label: label, Status: coreauth.StatusActive}
	ApplyCredentials(record, creds)
	return record
}

func ApplyCredentials(record *coreauth.Auth, creds Credentials) {
	if record == nil {
		return
	}
	creds = normalizeCredentials(record.Provider, creds)
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	if creds.ExpiresAt <= 0 {
		creds.ExpiresAt = CredentialsFromAuth(record).ExpiresAt
	}
	if record.Label == "" {
		record.Label = creds.Nickname
		if record.Label == "" {
			record.Label = creds.UID
		}
	}
	for key, value := range map[string]any{
		"type": record.Provider, "auth_kind": "oauth", "access_token": creds.AccessToken, "refresh_token": creds.RefreshToken,
		"personal_token": creds.PersonalToken, "uid": creds.UID, "nickname": creds.Nickname, "email": creds.Email,
		"user_type": creds.UserType, "organization_id": creds.OrganizationID, "organization_name": creds.OrganizationName,
		"realm": creds.Realm, "domain": creds.Domain, "machine_id": creds.MachineID, "expires_at": creds.ExpiresAt, "label": record.Label,
	} {
		record.Metadata[key] = value
	}
	if creds.ExpiresAt > 0 {
		record.Metadata["expired"] = time.Unix(creds.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
}

func CredentialsFromAuth(record *coreauth.Auth) Credentials {
	if record == nil {
		return Credentials{}
	}
	m := record.Metadata
	creds := Credentials{
		AccessToken: firstString(m, "access_token"), RefreshToken: firstString(m, "refresh_token"), PersonalToken: firstString(m, "personal_token"),
		UID: identity(m["uid"]), Nickname: firstString(m, "nickname"), Email: firstString(m, "email"), UserType: firstString(m, "user_type"),
		OrganizationID: identity(m["organization_id"]), OrganizationName: firstString(m, "organization_name"),
		Realm: firstString(m, "realm"), Domain: firstString(m, "domain"), MachineID: firstString(m, "machine_id"),
	}
	if seconds, ok := finiteNumber(m["expires_at"]); ok && seconds > 0 && seconds <= 253402300799 {
		creds.ExpiresAt = int64(seconds)
	}
	if creds.ExpiresAt == 0 {
		creds.ExpiresAt, _ = absoluteExpiry(m["expired"])
	}
	return normalizeCredentials(record.Provider, creds)
}

func EndpointForAuth(record *coreauth.Auth) string {
	if record == nil {
		return SiteForProvider(ProviderIntl).OpenAPI
	}
	if endpoint := firstString(record.Metadata, "openapi_url", "base_url"); endpoint != "" {
		return strings.TrimRight(endpoint, "/")
	}
	return SiteForProvider(record.Provider).OpenAPI
}

func GatewayForAuth(record *coreauth.Auth) string {
	if record == nil {
		return SiteForProvider(ProviderIntl).Gateway
	}
	if endpoint := firstString(record.Metadata, "gateway_url"); endpoint != "" {
		return strings.TrimRight(endpoint, "/")
	}
	return SiteForProvider(record.Provider).Gateway
}

func apiHeaders(provider string, creds Credentials) http.Header {
	req := &http.Request{}
	SignAPIRequest(req, provider, creds)
	return req.Header
}

// SignAPIRequest applies ordinary OpenAPI headers, with no COSY inference signature.
func SignAPIRequest(req *http.Request, provider string, creds Credentials) {
	if req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	site := SiteForProvider(provider)
	machineID := DeriveID(creds.UID, "machine")
	for name, value := range map[string]string{
		"Content-Type": "application/json", "Accept": "application/json, text/plain, */*", "User-Agent": site.userAgent(),
		"Authorization": "Bearer " + creds.AccessToken, "Origin": site.Website, "Referer": site.Website + "/",
		"X-Request-ID": strings.ReplaceAll(uuid.NewString(), "-", ""), "X-Machine-ID": machineID, "X-Session-ID": DeriveID(creds.UID, "session"),
	} {
		req.Header.Set(name, value)
	}
}

// DeriveID matches qoder_fingerprint.py, including its optional legacy encoding.
func DeriveID(uid, salt string) string {
	if uid == "" {
		uid = "anonymous"
	}
	digest := md5.Sum([]byte(salt + ":" + uid))
	h := hex.EncodeToString(digest[:])
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QD_DEVICE_ID_LEGACY"))) {
	case "1", "on", "true", "yes", "enable", "enabled":
		return h
	}
	h = h[:12] + "4" + h[13:16] + "a" + h[17:]
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func MachineToken(uid string) string {
	digest := sha512.Sum512([]byte("machinetoken:" + uid))
	return base64.RawURLEncoding.EncodeToString(digest[:])[:43]
}

func MachineType(uid string) string {
	return strings.ReplaceAll(DeriveID(uid, "machinetype"), "-", "")[:18]
}

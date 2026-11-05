package qoder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/idna"
)

const MachineIdentityMetadataKey = "qoder_machine_identity"

// MachineIdentity contains native UMID values, never derived substitutes.
type MachineIdentity struct {
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`
	MachineCode  string `json:"machineCode"`
	MachineOS    string `json:"machineOS"`
	Hostname     string `json:"hostname"`
}

// CheckInResult contains only public outcomes; messages never echo upstream data.
type CheckInResult struct {
	Status  string  `json:"status"`
	Claimed int     `json:"claimed"`
	Already int     `json:"already"`
	Credits float64 `json:"credits"`
	Message string  `json:"message"`
}

var ErrMachineIdentityUnavailable = errors.New("qoder: native machine identity unavailable; configure qoder_machine_identity or QODER_RUNTIME_INFO_PATH")

// MachineIdentityFromAuth accepts the JSON object stored on this account only.
func MachineIdentityFromAuth(auth *coreauth.Auth) *MachineIdentity {
	if auth == nil {
		return nil
	}
	raw, err := json.Marshal(auth.Metadata[MachineIdentityMetadataKey])
	if err != nil {
		return nil
	}
	var ident MachineIdentity
	if json.Unmarshal(raw, &ident) != nil {
		return nil
	}
	return normalizedMachineIdentity(&ident)
}

func safeIdentityHeader(value string) bool {
	if len(value) > 16384 {
		return false
	}
	for _, ch := range value {
		if ch < 32 || ch > 126 {
			return false
		}
	}
	return true
}

func normalizedMachineIdentity(ident *MachineIdentity) *MachineIdentity {
	if ident == nil {
		return nil
	}
	copy := *ident
	copy.MachineToken = strings.TrimSpace(copy.MachineToken)
	copy.MachineType = strings.TrimSpace(copy.MachineType)
	copy.MachineCode = strings.TrimSpace(copy.MachineCode)
	for _, value := range []string{copy.MachineToken, copy.MachineType, copy.MachineCode} {
		if value == "" || !safeIdentityHeader(value) {
			return nil
		}
	}
	if copy.MachineOS == "" {
		copy.MachineOS = desktopMachineOS()
	}
	if !safeIdentityHeader(copy.MachineOS) {
		return nil
	}
	if copy.Hostname == "" {
		copy.Hostname, _ = os.Hostname()
	}
	copy.Hostname = desktopHostname(copy.Hostname)
	return &copy
}

func desktopMachineOS() string {
	raw := strings.TrimSpace(os.Getenv("QD_COSY_MACHINE_OS"))
	if strings.EqualFold(raw, "auto") {
		arch, platform := "x86_64", runtime.GOOS
		if runtime.GOARCH == "arm64" {
			arch = "aarch64"
		}
		if platform == "windows" {
			platform = "win32"
		}
		return arch + "_" + platform
	}
	if raw != "" {
		return raw
	}
	if runtime.GOOS == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}

func desktopHostname(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return ""
	}
	hash := func(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))[:8] }
	if !safeIdentityHeader(name) {
		if ascii, err := idna.Lookup.ToASCII(name); err == nil && safeIdentityHeader(ascii) {
			name = ascii
		} else {
			var slug strings.Builder
			for _, ch := range name {
				if ch < 33 || ch > 126 {
					ch = '-'
				}
				slug.WriteRune(ch)
			}
			head := strings.Trim(slug.String(), "-")
			for strings.Contains(head, "--") {
				head = strings.ReplaceAll(head, "--", "-")
			}
			if head == "" {
				head = "unknown"
			}
			name = head + "-" + hash(name)
		}
	}
	if len(name) > 96 {
		name = strings.TrimRight(name[:87], "- 	") + "-" + hash(name)
	}
	return name
}

// runtimeInfoInput mirrors qoder_accounts.py:353-393 at upstream 45e76f9.
func runtimeInfoInput(provider, uid, goos string) ([]string, []byte) {
	region := "3"
	if provider == ProviderCN {
		region = "0"
	}
	args := []string{region}
	if uid != "" && (goos == "windows" || goos == "darwin") {
		args = append(args, "--account-stdin")
		data, _ := json.Marshal(map[string]string{"account": uid})
		return args, append(data, '\n')
	}
	return args, nil
}

// Do not embed bytes.Buffer: its promoted ReadFrom would bypass Write's limit
// when os/exec copies stdout through io.Copy.
type runtimeInfoOutput struct{ buffer bytes.Buffer }

func (b *runtimeInfoOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 64<<10 {
		return 0, ErrMachineIdentityUnavailable
	}
	return b.buffer.Write(p)
}

// NativeMachineIdentity runs only an explicitly supplied absolute local path. It
// never discovers installations, downloads components, or resets identity seeds.
func NativeMachineIdentity(ctx context.Context, provider, uid string) (*MachineIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(os.Getenv("QODER_RUNTIME_INFO_PATH"))
	if (provider != ProviderIntl && provider != ProviderCN) || !filepath.IsAbs(path) {
		return nil, ErrMachineIdentityUnavailable
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrMachineIdentityUnavailable
	}
	args, stdin := runtimeInfoInput(provider, uid, runtime.GOOS)
	acquireCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(acquireCtx, path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stderr = io.Discard
	var output runtimeInfoOutput
	cmd.Stdout = &output
	if errRun := cmd.Run(); errRun != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		return nil, ErrMachineIdentityUnavailable
	}
	line := bytes.SplitN(bytes.TrimSpace(output.buffer.Bytes()), []byte{'\n'}, 2)[0]
	var ident MachineIdentity
	if json.Unmarshal(line, &ident) != nil {
		return nil, ErrMachineIdentityUnavailable
	}
	if normalized := normalizedMachineIdentity(&ident); normalized != nil {
		return normalized, nil
	}
	return nil, ErrMachineIdentityUnavailable
}

func desktopCheckInHeaders(creds Credentials, ident *MachineIdentity) http.Header {
	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "Qoder")
	h.Set("Authorization", "Bearer "+creds.AccessToken)
	h.Set("Cosy-ClientType", "10")
	h.Set("Cosy-Version", "0.4.8")
	if ident != nil {
		// The desktop account ID is separate from the three native UMID values.
		h.Set("Cosy-MachineId", DeriveID(creds.UID, "machine"))
		h.Set("Cosy-MachineToken", ident.MachineToken)
		h.Set("Cosy-MachineType", ident.MachineType)
		h.Set("Cosy-MachineCode", ident.MachineCode)
		h.Set("Cosy-MachineOS", ident.MachineOS)
		if ident.Hostname != "" {
			h.Set("Cosy-MachineHostname", ident.Hostname)
		}
	}
	return h
}

func checkInResult(status, message string) CheckInResult {
	return CheckInResult{Status: status, Message: message}
}

// CheckIn claims only credit campaigns. A nil identity permits native sampling;
// an explicit empty identity disables sampling (CN can use basic desktop headers).
// Quota refresh and account scheduling are deliberately left to the caller.
func (s *Service) CheckIn(ctx context.Context, creds Credentials, identity *MachineIdentity) (CheckInResult, error) {
	failed := checkInResult("error", "Qoder check-in failed")
	if err := ctx.Err(); err != nil {
		return failed, err
	}
	if strings.TrimSpace(creds.AccessToken) == "" || strings.TrimSpace(creds.UID) == "" || (creds.Realm != "" && creds.Realm != s.site.Realm) {
		return failed, errors.New("qoder: invalid check-in account credentials")
	}
	ident := normalizedMachineIdentity(identity)
	if identity == nil {
		ident, _ = NativeMachineIdentity(ctx, s.site.Provider, creds.UID)
	}
	if err := ctx.Err(); err != nil {
		return failed, err
	}
	if ident == nil && s.site.Provider == ProviderIntl {
		return checkInResult("unavailable", "International Qoder check-in requires native identity; configure qoder_machine_identity or QODER_RUNTIME_INFO_PATH"), nil
	}
	headers := desktopCheckInHeaders(creds, ident)
	data, status, err := s.checkInCall(ctx, http.MethodGet, "/sash/api/v1/me/campaigns", headers)
	if missingCheckInAPI(status) {
		return s.legacyCheckIn(ctx, creds)
	}
	if checkInSignal(data, "BLOCKED") || checkInSignal(data, "SAME_PERSON_ALREADY_CLAIMED") {
		return checkInResult("blocked", "Qoder blocked credit campaigns for this person or device"), nil
	}
	if err != nil {
		return failed, err
	}
	if checkInRejected(data) {
		return failed, errors.New("qoder: campaign query rejected")
	}
	if data["showCampaign"] == false {
		return checkInResult("unavailable", "Qoder did not make campaigns available for this account and identity"), nil
	}
	items, ok := data["campaigns"].([]any)
	if !ok {
		return failed, errors.New("qoder: invalid campaign list")
	}
	// Validate before the first mutation, including conflicting duplicate rows.
	seen := make(map[string]map[string]any)
	var campaigns []map[string]any
	for _, item := range items {
		campaign, valid := item.(map[string]any)
		if !valid || !validCampaignFields(campaign) {
			return failed, errors.New("qoder: invalid campaign entry")
		}
		id := campaignID(campaign)
		if id == "" || id == "." || id == ".." {
			return failed, errors.New("qoder: campaign identity missing")
		}
		if previous, exists := seen[id]; exists {
			a, _ := json.Marshal(previous)
			b, _ := json.Marshal(campaign)
			if !bytes.Equal(a, b) {
				return failed, errors.New("qoder: inconsistent duplicate campaign")
			}
			continue
		}
		seen[id] = campaign
		campaigns = append(campaigns, campaign)
	}
	result := checkInResult("skipped", "No claimable credit campaigns")
	blocked := false
	for _, campaign := range campaigns {
		benefit, _ := campaign["benefit"].(map[string]any)
		kind := strings.ToUpper(firstString(benefit, "kind"))
		if kind != "" && kind != "CREDITS" {
			continue
		}
		action := strings.ToUpper(firstString(campaign, "actionType", "action_type"))
		claimStatus := strings.ToUpper(firstString(campaign, "claimStatus", "claim_status"))
		if claimStatus == "BLOCKED" || checkInSignal(campaign, "SAME_PERSON_ALREADY_CLAIMED") {
			blocked = true
			continue
		}
		if claimStatus == "CLAIMED" {
			amount, _ := finiteNumber(benefit["amount"])
			if action == "" || action == "CLAIM_BENEFIT" || amount > 0 {
				result.Already++
			}
			continue
		}
		if action != "" && action != "CLAIM_BENEFIT" {
			continue
		}
		if claimStatus != "CLAIMABLE" {
			continue
		}
		claim, code, errClaim := s.checkInCall(ctx, http.MethodPost, "/sash/api/v1/me/campaigns/"+url.PathEscape(campaignID(campaign))+"/claim", headers)
		outcome, errOutcome := interpretCheckInClaim(claim, code, errClaim, false)
		result.Claimed += outcome.Claimed
		result.Already += outcome.Already
		total := result.Credits + outcome.Credits
		if errOutcome != nil || math.IsInf(total, 0) {
			result.Status, result.Message = "error", "Qoder could not complete all credit claims"
			if errOutcome == nil {
				errOutcome = errors.New("qoder: invalid credit total")
			}
			return result, errOutcome
		}
		result.Credits = total
		blocked = blocked || outcome.Status == "blocked"
	}
	switch {
	case blocked:
		result.Status, result.Message = "blocked", "Qoder blocked a claim; this person or device may already have claimed this round"
	case result.Claimed > 0:
		result.Status, result.Message = "success", "Credit rewards claimed"
	case result.Already > 0:
		result.Status, result.Message = "already", "Credit rewards already claimed"
	}
	return result, nil
}

func campaignID(campaign map[string]any) string {
	for _, key := range []string{"campaignId", "campaign_id", "id"} {
		if id := identity(campaign[key]); id != "" {
			return id
		}
	}
	return ""
}

func validCampaignFields(campaign map[string]any) bool {
	for _, key := range []string{"actionType", "action_type", "claimStatus", "claim_status"} {
		if value := campaign[key]; value != nil {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	if value := campaign["benefit"]; value != nil {
		benefit, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if kind := benefit["kind"]; kind != nil {
			if _, ok = kind.(string); !ok {
				return false
			}
		}
	}
	return true
}

func missingCheckInAPI(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusGone
}

func checkInSignal(data map[string]any, wanted string) bool {
	for _, key := range []string{"status", "result", "failureCode", "errorCode", "code", "error", "unavailableReason", "unavailable_reason"} {
		if strings.EqualFold(firstString(data, key), wanted) {
			return true
		}
	}
	if nested, ok := data["error"].(map[string]any); ok {
		return checkInSignal(nested, wanted)
	}
	return false
}

func checkInRejected(data map[string]any) bool {
	if data["success"] == false || (data["error"] != nil && data["error"] != "") || firstString(data, "failureCode", "errorCode") != "" {
		return true
	}
	if code, exists := data["code"]; exists && code != nil {
		number, ok := finiteNumber(code)
		return !ok || number != 0
	}
	return false
}

func interpretCheckInClaim(data map[string]any, status int, err error, legacy bool) (CheckInResult, error) {
	failed := checkInResult("error", "Qoder credit claim failed")
	// A same-person block must never be converted into an own-account replay.
	if checkInSignal(data, "BLOCKED") || checkInSignal(data, "SAME_PERSON_ALREADY_CLAIMED") {
		return checkInResult("blocked", "Qoder blocked this person or device from claiming again"), nil
	}
	if status == http.StatusConflict {
		return CheckInResult{Status: "already", Already: 1, Message: "Credit reward already claimed"}, nil
	}
	if data["replayed"] == true || checkInSignal(data, "ALREADY_CLAIMED") || checkInSignal(data, "REPLAYED") {
		return CheckInResult{Status: "already", Already: 1, Message: "Credit reward already claimed"}, nil
	}
	if err != nil {
		return failed, err
	}
	if replayed := data["replayed"]; replayed != nil {
		if _, ok := replayed.(bool); !ok {
			return failed, errors.New("qoder: invalid claim replay status")
		}
	}
	if checkInRejected(data) {
		return failed, errors.New("qoder: credit claim rejected")
	}
	state := strings.ToUpper(firstString(data, "status", "result"))
	hasLegacyReward := data["rewardCredits"] != nil
	legacyConfirmed := legacy && state == "" && hasLegacyReward &&
		(data["success"] == nil || data["success"] == true)
	if state != "CLAIMED" && state != "GRANTED" && state != "SUCCESS" && !legacyConfirmed {
		return failed, errors.New("qoder: credit claim was not confirmed")
	}
	if !validCampaignFields(data) {
		return failed, errors.New("qoder: invalid claim benefit")
	}
	benefit, _ := data["benefit"].(map[string]any)
	if kind := strings.ToUpper(firstString(benefit, "kind")); kind != "" && kind != "CREDITS" {
		return failed, errors.New("qoder: claim returned a non-credit benefit")
	}
	credits := 0.0
	for _, amount := range []any{benefit["amount"], data["rewardCredits"], data["amount"], data["credits"]} {
		if amount == nil {
			continue
		}
		var ok bool
		credits, ok = finiteNumber(amount)
		if !ok || credits < 0 {
			return failed, errors.New("qoder: invalid credit reward")
		}
		break
	}
	return CheckInResult{Status: "success", Claimed: 1, Credits: credits, Message: "Credit reward claimed"}, nil
}

func (s *Service) legacyCheckIn(ctx context.Context, creds Credentials) (CheckInResult, error) {
	headers := apiHeaders(s.site.Provider, creds)
	data, status, err := s.checkInCall(ctx, http.MethodGet, "/sash/api/v1/me/daily-check-in/status", headers)
	if missingCheckInAPI(status) {
		return checkInResult("unavailable", "Qoder campaign and legacy daily check-in APIs are unavailable in this region"), nil
	}
	if checkInSignal(data, "BLOCKED") || checkInSignal(data, "SAME_PERSON_ALREADY_CLAIMED") {
		return checkInResult("blocked", "Qoder daily credit claim is blocked"), nil
	}
	if err != nil {
		return checkInResult("error", "Qoder daily check-in query failed"), err
	}
	if checkInRejected(data) {
		return checkInResult("error", "Qoder daily check-in query rejected"), errors.New("qoder: daily check-in query rejected")
	}
	switch strings.ToUpper(firstString(data, "status")) {
	case "CLAIMED":
		return CheckInResult{Status: "already", Already: 1, Message: "Daily credit reward already claimed"}, nil
	case "CLAIMABLE":
		claim, code, errClaim := s.checkInCall(ctx, http.MethodPost, "/sash/api/v1/me/daily-check-in/claim", headers)
		if missingCheckInAPI(code) {
			return checkInResult("unavailable", "Qoder legacy daily claim API is unavailable in this region"), nil
		}
		return interpretCheckInClaim(claim, code, errClaim, true)
	case "BLOCKED":
		return checkInResult("blocked", "Qoder daily credit claim is blocked"), nil
	case "":
		return checkInResult("error", "Qoder returned no daily check-in status"), errors.New("qoder: daily check-in status missing")
	default:
		return checkInResult("skipped", "Qoder daily credit reward is not claimable"), nil
	}
}

// checkInCall retains structured conflict details without exposing response text.
func (s *Service) checkInCall(ctx context.Context, method, path string, headers http.Header) (map[string]any, int, error) {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.site.OpenAPI+path, body)
	if err != nil {
		return nil, 0, errors.New("qoder: invalid check-in endpoint")
	}
	req.Header = headers.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, errRequest := client.Do(req)
	if errRequest != nil {
		if errContext := ctx.Err(); errContext != nil {
			return nil, 0, errContext
		}
		return nil, 0, errors.New("qoder: check-in request failed")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("qoder: close check-in response failed")
		}
	}()
	status := response.StatusCode
	if missingCheckInAPI(status) {
		return nil, status, &APIError{Status: status}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	decoder.UseNumber()
	var data map[string]any
	errDecode := decoder.Decode(&data)
	if errDecode == nil {
		if decoder.Decode(new(any)) != io.EOF {
			errDecode = errors.New("trailing data")
		}
	}
	if errDecode != nil {
		data = nil
	}
	if errContext := ctx.Err(); errContext != nil {
		return data, status, errContext
	}
	if status < 200 || status >= 300 {
		return data, status, &APIError{Status: status}
	}
	if errDecode != nil || data == nil {
		return nil, status, errors.New("qoder: invalid check-in response")
	}
	return data, status, nil
}

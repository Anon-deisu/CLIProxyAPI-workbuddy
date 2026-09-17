package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const PrismDefaultBaseURL = "https://prism.openai.com"

const prismResourceRefreshLead = time.Minute

type PrismHTTPError struct {
	Code           int
	Message        string
	RefreshSandbox bool
}

func (e *PrismHTTPError) Error() string   { return e.Message }
func (e *PrismHTTPError) StatusCode() int { return e.Code }

func ShouldRefreshPrismSandbox(err error) bool {
	var prismErr *PrismHTTPError
	return errors.As(err, &prismErr) && prismErr.RefreshSandbox
}

type PrismClient struct {
	mu                sync.Mutex
	http              *http.Client
	baseURL           string
	projectName       string
	projectID         string
	userID            string
	sandbox           prismSandbox
	accessToken       string
	resourcesExpireAt time.Time
	pollInterval      time.Duration
}

type prismSandbox struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	SessionID string `json:"sandbox_session_id"`
}

type prismTurn struct {
	Status         string          `json:"status"`
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id"`
	State          json.RawMessage `json:"turn_state"`
	Response       struct {
		Status  string          `json:"status"`
		Payload json.RawMessage `json:"payload"`
	} `json:"response"`
}

type PrismRun struct {
	client         *PrismClient
	turn           prismTurn
	conversationID string
	sandboxToken   string
}

func NewPrismClient(httpClient *http.Client, baseURL, projectName string) *PrismClient {
	client := *httpClient
	client.Jar, _ = cookiejar.New(nil)
	return &PrismClient{http: &client, baseURL: strings.TrimRight(baseURL, "/"), projectName: projectName, pollInterval: 3 * time.Second}
}

func (c *PrismClient) request(ctx context.Context, method, path string, body, out any, sandboxToken string) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Prism request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.baseURL)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sandboxToken != "" {
		req.Header.Set("X-Crixet-Sandbox-Token", sandboxToken)
	}
	resp, errDo := c.http.Do(req)
	if errDo != nil {
		return fmt.Errorf("Prism %s: %w", path, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("close Prism response")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &PrismHTTPError{Code: resp.StatusCode, Message: fmt.Sprintf("Prism %s returned HTTP %d", path, resp.StatusCode)}
	}
	if out != nil {
		if errDecode := json.NewDecoder(resp.Body).Decode(out); errDecode != nil {
			return fmt.Errorf("decode Prism %s: %w", path, errDecode)
		}
	}
	return nil
}

func (c *PrismClient) prepare(ctx context.Context, accessToken string) error {
	origin, err := url.Parse(c.baseURL)
	if err != nil {
		return err
	}
	c.http.Jar.SetCookies(origin, []*http.Cookie{{Name: "prism_oai_access_token", Value: accessToken, Path: "/", Secure: origin.Scheme == "https", HttpOnly: true}})
	var auth struct {
		User struct {
			ID        string `json:"id"`
			Anonymous bool   `json:"is_anonymous"`
		} `json:"user"`
		Tier string `json:"userTier"`
	}
	if err = c.request(ctx, http.MethodGet, "/auth/session", nil, &auth, ""); err != nil {
		return err
	}
	if auth.Tier != "logged_in" || auth.User.ID == "" || auth.User.Anonymous {
		return &PrismHTTPError{Code: http.StatusUnauthorized, Message: "Prism could not restore the OpenAI session"}
	}
	c.userID = auth.User.ID
	c.accessToken = accessToken
	if c.projectID == "" {
		var list struct {
			Projects []struct {
				ID    string `json:"uuid"`
				Title string `json:"title"`
			} `json:"projects"`
		}
		if err = c.request(ctx, http.MethodGet, "/api/projects", nil, &list, ""); err != nil {
			return err
		}
		for _, project := range list.Projects {
			if project.Title == c.projectName {
				c.projectID = project.ID
				break
			}
		}
		if c.projectID == "" {
			var created struct {
				ID string `json:"uuid"`
			}
			body := map[string]any{"project_uuid": uuid.NewString(), "title": c.projectName, "file_uuids": []string{}}
			if err = c.request(ctx, http.MethodPost, "/api/projects", body, &created, ""); err != nil {
				return err
			}
			if created.ID == "" {
				return fmt.Errorf("Prism returned an empty project ID")
			}
			c.projectID = created.ID
		}
	}
	if c.sandbox.Token != "" && time.Now().Add(prismResourceRefreshLead).Before(c.resourcesExpireAt) {
		return nil
	}
	sandbox := c.sandbox
	if sandbox.Token == "" {
		if err = c.request(ctx, http.MethodPost, "/api/backend/1/new", nil, &sandbox, ""); err != nil {
			return err
		}
	}
	if sandbox.URL == "" || sandbox.Token == "" {
		return fmt.Errorf("Prism returned incomplete sandbox credentials")
	}
	var resource struct {
		Token     string `json:"access_token"`
		BaseURL   string `json:"resources_base_url"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err = c.request(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(c.projectID)+"/sandbox/resources-token", map[string]any{"sandbox_session_id": nil, "sandbox_token": sandbox.Token}, &resource, ""); err != nil {
		return err
	}
	if err = c.request(ctx, http.MethodPost, "/s/sandboxes/proxy/resources-token", map[string]any{"token": resource.Token, "resourceBaseUrl": resource.BaseURL, "projectId": c.projectID}, nil, sandbox.Token); err != nil {
		return err
	}
	var document json.RawMessage
	if err = c.request(ctx, http.MethodPost, "/api/y", map[string]string{"docId": c.projectID}, &document, ""); err != nil {
		return err
	}
	if err = c.request(ctx, http.MethodPost, "/s/sandboxes/proxy/token", document, nil, sandbox.Token); err != nil {
		return err
	}
	for {
		var syncState struct {
			Status string `json:"status"`
		}
		if err = c.request(ctx, http.MethodGet, "/s/sandboxes/proxy/wait-for-sync?wait_ms=10000", nil, &syncState, sandbox.Token); err != nil {
			return err
		}
		if syncState.Status == "synced" {
			c.sandbox = sandbox
			c.resourcesExpireAt = time.Unix(resource.ExpiresAt, 0)
			return nil
		}
		if syncState.Status != "syncing" {
			return &PrismHTTPError{Code: http.StatusServiceUnavailable, Message: "Prism sandbox synchronization failed"}
		}
	}
}

func (c *PrismClient) Start(ctx context.Context, accessToken, model, effort string, input json.RawMessage) (*PrismRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepare(ctx, accessToken); err != nil {
		return nil, err
	}
	// Each generation has a fresh upstream transcript; the client supplies its complete history.
	conversationID := "cdx1_" + uuid.NewString()
	body := map[string]any{"input": input, "conversationId": conversationID, "metadata": map[string]string{
		"projectId": c.projectID, "userId": c.userID, "model": model, "reasoning_effort": effort,
		"frontend_origin": c.baseURL, "sandbox_url": strings.TrimRight(c.sandbox.URL, "/") + "/", "sandbox_token": c.sandbox.Token,
	}}
	run := &PrismRun{client: c, conversationID: conversationID, sandboxToken: c.sandbox.Token}
	if err := c.request(ctx, http.MethodPost, "/api/llm/response_with_tools_start", body, &run.turn, ""); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *PrismRun) Wait(ctx context.Context) (json.RawMessage, error) {
	stage := "start"
	for r.turn.Status != "completed" {
		if r.turn.Status != "started" && r.turn.Status != "pending" {
			return nil, fmt.Errorf("invalid Prism turn status %q", r.turn.Status)
		}
		if r.client.pollInterval > 0 {
			timer := time.NewTimer(r.client.pollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		var next prismTurn
		if errRefresh := r.refreshResources(ctx); errRefresh != nil {
			return nil, errRefresh
		}
		stage = "status"
		if err := r.client.request(ctx, http.MethodPost, "/api/llm/response_with_tools_status", map[string]any{"request_id": r.turn.RequestID, "turn_state": r.turn.State}, &next, ""); err != nil {
			return nil, err
		}
		r.turn = next
	}
	if r.turn.Response.Status != "success" {
		var failure struct{ Reason, Message string }
		if err := json.Unmarshal(r.turn.Response.Payload, &failure); err != nil {
			return nil, fmt.Errorf("invalid Prism failure response: %w", err)
		}
		code := http.StatusBadGateway
		refreshSandbox := false
		switch failure.Reason {
		case "sandbox_reconnecting":
			code = http.StatusServiceUnavailable
			refreshSandbox = true
		case "unauthorized", "session_expired":
			code = http.StatusUnauthorized
		case "rate_limit_exceeded", "quota_exceeded":
			code = http.StatusTooManyRequests
		}
		messageLower := strings.ToLower(failure.Message)
		if failure.Reason == "unknown" && strings.Contains(messageLower, "error while processing conversation") {
			code = http.StatusServiceUnavailable
			refreshSandbox = true
		}
		if refreshSandbox {
			r.client.mu.Lock()
			if r.client.sandbox.Token == r.sandboxToken {
				r.client.sandbox = prismSandbox{}
				r.client.resourcesExpireAt = time.Time{}
			}
			r.client.mu.Unlock()
		}
		return nil, &PrismHTTPError{Code: code, Message: fmt.Sprintf("Prism generation failed: %s: %s (stage=%s request_id=%s conversation_id=%s)", failure.Reason, failure.Message, stage, r.turn.RequestID, r.conversationID), RefreshSandbox: refreshSandbox}
	}
	return r.turn.Response.Payload, nil
}

func (r *PrismRun) refreshResources(ctx context.Context) error {
	c := r.client
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sandbox.Token == r.sandboxToken && !time.Now().Add(prismResourceRefreshLead).Before(c.resourcesExpireAt) {
		return c.prepare(ctx, c.accessToken)
	}
	return nil
}

func (r *PrismRun) Stop(ctx context.Context) error {
	if r.turn.Status == "completed" {
		return nil
	}
	return r.client.request(ctx, http.MethodPost, "/api/llm/response_with_tools_stop", map[string]any{
		"request_id": r.turn.RequestID, "conversation_id": r.conversationID, "turn_state": r.turn.State,
	}, nil, "")
}

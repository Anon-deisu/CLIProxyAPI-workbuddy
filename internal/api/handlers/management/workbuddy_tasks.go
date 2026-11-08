package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const workBuddyTaskPollInterval = 30 * time.Minute
const workBuddyDefaultCheckinIntervalSeconds = 5

type WorkBuddyTaskService struct {
	mu       sync.Mutex
	manager  *coreauth.Manager
	cfg      *config.Config
	status   map[string]workbuddy.AccountTaskStatus
	lastRuns map[string]workbuddy.TaskRun
	inFlight map[string]chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	wg       sync.WaitGroup
	wait     func(context.Context, time.Duration) error
}

func NewWorkBuddyTaskService(cfg *config.Config, manager *coreauth.Manager) *WorkBuddyTaskService {
	ctx, cancel := context.WithCancel(context.Background())
	return &WorkBuddyTaskService{manager: manager, cfg: cfg.CloneForRuntime(), status: make(map[string]workbuddy.AccountTaskStatus), lastRuns: make(map[string]workbuddy.TaskRun), inFlight: make(map[string]chan struct{}), ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), wait: waitWorkBuddyTaskInterval}
}

func (s *WorkBuddyTaskService) Start() {
	if s == nil || s.manager == nil || s.manager.HomeEnabled() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				s.runAutomatic()
				timer.Reset(workBuddyTaskPollInterval)
			case <-s.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				s.runAutomatic()
				timer.Reset(workBuddyTaskPollInterval)
			}
		}
	}()
}

func (s *WorkBuddyTaskService) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *WorkBuddyTaskService) Configure(cfg *config.Config) {
	if s == nil {
		return
	}
	s.mu.Lock()
	oldCheckin := s.cfg != nil && s.cfg.Routing.WorkBuddyAutoCheckin != nil && *s.cfg.Routing.WorkBuddyAutoCheckin
	oldGrowth := s.cfg != nil && s.cfg.Routing.WorkBuddyAutoTasks != nil && *s.cfg.Routing.WorkBuddyAutoTasks
	s.cfg = cfg.CloneForRuntime()
	newCheckin := cfg != nil && cfg.Routing.WorkBuddyAutoCheckin != nil && *cfg.Routing.WorkBuddyAutoCheckin
	newGrowth := cfg != nil && cfg.Routing.WorkBuddyAutoTasks != nil && *cfg.Routing.WorkBuddyAutoTasks
	s.mu.Unlock()
	if (!oldCheckin && newCheckin) || (!oldGrowth && newGrowth) {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *WorkBuddyTaskService) configuration() (*config.Config, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return nil, false, false
	}
	return s.cfg.CloneForRuntime(), s.cfg.Routing.WorkBuddyAutoCheckin != nil && *s.cfg.Routing.WorkBuddyAutoCheckin, s.cfg.Routing.WorkBuddyAutoTasks != nil && *s.cfg.Routing.WorkBuddyAutoTasks
}

func (s *WorkBuddyTaskService) accounts() []*coreauth.Auth {
	if s == nil || s.manager == nil || s.manager.HomeEnabled() {
		return nil
	}
	auths := s.manager.List()
	result := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if auth.Provider == workbuddy.ProviderCN || auth.Provider == workbuddy.ProviderAI {
			result = append(result, auth)
		}
	}
	return result
}

func (s *WorkBuddyTaskService) acquire(id string) (chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, false
	}
	if busy := s.inFlight[id]; busy != nil {
		return busy, false
	}
	done := make(chan struct{})
	s.inFlight[id] = done
	s.wg.Add(1)
	return done, true
}

func (s *WorkBuddyTaskService) release(id string, done chan struct{}) {
	s.mu.Lock()
	if s.inFlight[id] == done {
		delete(s.inFlight, id)
		close(done)
	}
	s.mu.Unlock()
	s.wg.Done()
}

func (s *WorkBuddyTaskService) prepare(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return s.manager.PrepareRequestAuth(ctx, workBuddyQuotaPreparer{cfg}, auth)
}

func (s *WorkBuddyTaskService) Status(ctx context.Context, auth *coreauth.Auth) (workbuddy.AccountTaskStatus, error) {
	cfg, _, _ := s.configuration()
	if cfg == nil {
		return workbuddy.AccountTaskStatus{}, fmt.Errorf("configuration unavailable")
	}
	prepared, errPrepare := s.prepare(ctx, cfg, auth)
	if errPrepare != nil {
		return workbuddy.AccountTaskStatus{}, errPrepare
	}
	transport := (&Handler{cfg: cfg}).apiCallTransport(prepared, "")
	status, errStatus := workbuddy.NewTaskClient(prepared, &http.Client{Transport: transport}).Status(ctx, prepared)
	s.mu.Lock()
	s.status[prepared.ID] = status
	if previous := s.lastRuns[prepared.ID]; previous.Trigger != "" {
		copyRun := previous
		copyRun.Auth = nil
		status.LastRun = &copyRun
	}
	status.Running = s.inFlight[prepared.ID] != nil
	s.mu.Unlock()
	return status, errStatus
}

func (s *WorkBuddyTaskService) Cached(auth *coreauth.Auth) workbuddy.AccountTaskStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status[auth.ID]
	status.AuthIndex = auth.Index
	status.Name = auth.FileName
	status.Realm = "cn"
	if auth.Provider == workbuddy.ProviderAI {
		status.Realm = "intl"
	}
	creds := workbuddy.CredentialsFromAuth(auth)
	status.Personal = creds.AccessToken != "" && creds.RefreshToken != "" && creds.UID != "" && creds.EnterpriseID == ""
	status.Checkin.Supported = status.Personal && auth.Provider == workbuddy.ProviderCN
	status.Activity.Supported = status.Personal
	status.Growth.Supported = status.Checkin.Supported
	checkedAt, _ := time.Parse(time.RFC3339, status.CheckedAt)
	if workbuddy.CheckinDay(checkedAt) != workbuddy.CheckinDay(time.Now()) {
		status.Checkin.CheckedIn = false
		status.Checkin.TodayCredit = 0
	}
	if workbuddy.CheckinCompletedToday(auth, time.Now()) {
		status.Checkin.CheckedIn = true
	}
	status.Running = s.inFlight[auth.ID] != nil
	if previous := s.lastRuns[auth.ID]; previous.Trigger != "" {
		copyRun := previous
		copyRun.Auth = nil
		status.LastRun = &copyRun
	}
	return status
}

func (s *WorkBuddyTaskService) execute(auth *coreauth.Auth, checkin, growth bool, trigger string) bool {
	if trigger == "auto" {
		current, exists := s.manager.GetByID(auth.ID)
		if !exists || current.Disabled {
			return false
		}
		auth = current
		_, autoCheckin, autoGrowth := s.configuration()
		checkin = checkin && autoCheckin && auth.Provider == workbuddy.ProviderCN && !workbuddy.CheckinCompletedToday(auth, time.Now())
		growth = growth && autoGrowth
		if !checkin && !growth {
			return false
		}
	}
	done, acquired := s.acquire(auth.ID)
	if !acquired {
		return false
	}
	run := func() {
		defer s.release(auth.ID, done)
		cfg, autoCheckin, autoGrowth := s.configuration()
		if cfg == nil {
			return
		}
		if trigger == "auto" {
			checkin, growth = checkin && autoCheckin, growth && autoGrowth
		}
		if !checkin && !growth {
			return
		}
		prepared, errPrepare := s.prepare(s.ctx, cfg, auth)
		if errPrepare != nil {
			log.WithError(errPrepare).WithField("auth_id", auth.ID).Warn("workbuddy: task token refresh failed")
			now := time.Now().UTC().Format(time.RFC3339)
			s.mu.Lock()
			s.lastRuns[auth.ID] = workbuddy.TaskRun{Trigger: trigger, StartedAt: now, FinishedAt: now, Message: errPrepare.Error(), Results: []workbuddy.TaskResult{}}
			s.mu.Unlock()
			return
		}
		transport := (&Handler{cfg: cfg}).apiCallTransport(prepared, "")
		client := workbuddy.NewTaskClient(prepared, &http.Client{Transport: transport})
		result, errRun := client.Run(s.ctx, prepared, checkin, growth, trigger)
		if result.Auth != nil && (result.Auth.Metadata["workbuddy_last_activity_at"] != prepared.Metadata["workbuddy_last_activity_at"] || result.Auth.Metadata["workbuddy_last_checkin_at"] != prepared.Metadata["workbuddy_last_checkin_at"]) {
			updated, _, errPersist := s.manager.Modify(s.ctx, prepared.ID, prepared.RegistrationEpoch, func(current *coreauth.Auth) error {
				for _, key := range []string{"workbuddy_last_activity_at", "workbuddy_last_checkin_at"} {
					if result.Auth.Metadata[key] != prepared.Metadata[key] {
						current.Metadata[key] = result.Auth.Metadata[key]
					}
				}
				return nil
			})
			if errPersist != nil {
				log.WithError(errPersist).WithField("auth_id", auth.ID).Warn("workbuddy: task result was not persisted")
				result.OK = false
				result.Message = "任务状态保存失败：" + errPersist.Error()
			} else if updated != nil {
				prepared = updated
			}
		}
		result.Auth = nil
		if errRun != nil {
			result.OK = false
			result.Message = errRun.Error()
		}
		s.mu.Lock()
		s.lastRuns[auth.ID] = result
		if cached := s.status[auth.ID]; cached.AuthIndex != "" {
			cached.LastRun = &result
			cached.Running = false
			s.status[auth.ID] = cached
		}
		s.mu.Unlock()
		status, errStatus := s.Status(s.ctx, prepared)
		if errStatus != nil {
			log.WithError(errStatus).WithField("auth_id", auth.ID).Debug("workbuddy: task status refresh failed")
			return
		} else {
			log.WithFields(log.Fields{"auth_id": auth.ID, "trigger": trigger, "earned_credit": result.EarnedCredit, "ok": result.OK}).Info("workbuddy: account task cycle completed")
		}
		s.mu.Lock()
		status.Running = false
		s.status[auth.ID] = status
		s.mu.Unlock()
	}
	if trigger == "auto" {
		run()
	} else {
		go run()
	}
	return true
}

func waitWorkBuddyTaskInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *WorkBuddyTaskService) runAutomatic() {
	previousCheckin := false
	for _, auth := range s.accounts() {
		cfg, checkin, growth := s.configuration()
		if (!checkin && !growth) || s.ctx.Err() != nil {
			return
		}
		creds := workbuddy.CredentialsFromAuth(auth)
		if auth.Disabled || creds.AccessToken == "" || creds.RefreshToken == "" || creds.UID == "" || creds.EnterpriseID != "" || (auth.Provider == workbuddy.ProviderAI && !growth) {
			continue
		}
		checkin = checkin && auth.Provider == workbuddy.ProviderCN && !workbuddy.CheckinCompletedToday(auth, time.Now())
		if !checkin && !growth {
			continue
		}
		if previousCheckin && checkin {
			seconds := workBuddyDefaultCheckinIntervalSeconds
			if cfg.Routing.WorkBuddyCheckinIntervalSeconds != nil {
				seconds = *cfg.Routing.WorkBuddyCheckinIntervalSeconds
			}
			if errWait := s.wait(s.ctx, time.Duration(seconds)*time.Second); errWait != nil {
				return
			}
		}
		if s.execute(auth, checkin, growth, "auto") && checkin && auth.Provider == workbuddy.ProviderCN {
			previousCheckin = true
		}
	}
}

func (h *Handler) workBuddyTaskAccount(c *gin.Context) (*coreauth.Auth, bool) {
	if h == nil || h.authManager == nil || h.workBuddyTasks == nil || h.authManager.HomeEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "WorkBuddy task service unavailable"})
		return nil, false
	}
	index := strings.TrimSpace(c.Query("auth_index"))
	if index == "" {
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if c.Request.Body != nil {
			if errBind := c.ShouldBindJSON(&body); errBind != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
				return nil, false
			}
			index = strings.TrimSpace(body.AuthIndex)
		}
	}
	if index == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return nil, false
	}
	auth := h.authByIndex(index)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return nil, false
	}
	if auth.Provider != workbuddy.ProviderCN && auth.Provider != workbuddy.ProviderAI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential is not a WorkBuddy account"})
		return nil, false
	}
	return auth, true
}

// GetWorkBuddyTasks returns upstream progress and the latest explicit or automatic execution results.
func (h *Handler) GetWorkBuddyTasks(c *gin.Context) {
	auth, ok := h.workBuddyTaskAccount(c)
	if !ok {
		return
	}
	if c.Query("refresh") == "false" {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, h.workBuddyTasks.Cached(auth))
		return
	}
	status, errStatus := h.workBuddyTasks.Status(c.Request.Context(), auth)
	c.Header("Cache-Control", "no-store")
	if errStatus != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errStatus.Error(), "status": status})
		return
	}
	c.JSON(http.StatusOK, status)
}

func (h *Handler) RunWorkBuddyTasks(c *gin.Context) {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Checkin   bool   `json:"checkin"`
		Growth    bool   `json:"growth"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	auth, ok := h.workBuddyTaskAccountWithIndex(c, body.AuthIndex)
	if !ok {
		return
	}
	if !body.Checkin && !body.Growth {
		body.Checkin, body.Growth = true, true
	}
	started := h.workBuddyTasks.execute(auth, body.Checkin, body.Growth, "manual")
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusAccepted, gin.H{"status": "running", "message": map[bool]string{true: "已开始执行", false: "任务正在执行"}[started], "account": h.workBuddyTasks.Cached(auth)})
}

func (h *Handler) workBuddyTaskAccountWithIndex(c *gin.Context, index string) (*coreauth.Auth, bool) {
	if h == nil || h.authManager == nil || h.workBuddyTasks == nil || h.authManager.HomeEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "WorkBuddy task service unavailable"})
		return nil, false
	}
	auth := h.authByIndex(strings.TrimSpace(index))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return nil, false
	}
	if auth.Provider != workbuddy.ProviderCN && auth.Provider != workbuddy.ProviderAI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential is not a WorkBuddy account"})
		return nil, false
	}
	return auth, true
}

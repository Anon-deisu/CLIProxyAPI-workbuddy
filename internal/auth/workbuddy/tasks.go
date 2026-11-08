package workbuddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	taskChinaBillingBase = "https://www.codebuddy.cn"
	taskChinaWebBase     = "https://www.workbuddy.cn"
)

type TaskCheckin struct {
	Supported   bool    `json:"supported"`
	CheckedIn   bool    `json:"checked_in"`
	StreakDays  int     `json:"streak_days"`
	DailyCredit float64 `json:"daily_credit"`
	TodayCredit float64 `json:"today_credit"`
	Message     string  `json:"message,omitempty"`
}

type TaskActivity struct {
	Supported  bool   `json:"supported"`
	Completed  bool   `json:"completed"`
	StreakDays int    `json:"streak_days"`
	LastRunAt  string `json:"last_run_at,omitempty"`
	Message    string `json:"message,omitempty"`
}

type GrowthTask struct {
	Code         string `json:"code"`
	spec         taskSpec
	Name         string  `json:"name"`
	Description  string  `json:"description,omitempty"`
	Status       string  `json:"status"`
	Type         string  `json:"type,omitempty"`
	Current      int     `json:"current"`
	Target       int     `json:"target"`
	RewardCredit float64 `json:"reward_credit"`
	RewardEnergy float64 `json:"reward_energy"`
	Automatable  bool    `json:"automatable"`
	ManualReason string  `json:"manual_reason,omitempty"`
}

type GrowthStatus struct {
	Supported  bool         `json:"supported"`
	StreakDays int          `json:"streak_days"`
	Completed  int          `json:"completed"`
	Total      int          `json:"total"`
	Message    string       `json:"message,omitempty"`
	Tasks      []GrowthTask `json:"tasks"`
}

type TaskResult struct {
	Code         string  `json:"code"`
	Name         string  `json:"name"`
	OK           bool    `json:"ok"`
	Status       string  `json:"status"`
	Current      int     `json:"current"`
	Target       int     `json:"target"`
	RewardCredit float64 `json:"reward_credit"`
	Message      string  `json:"message,omitempty"`
}

type TaskRun struct {
	OK           bool           `json:"ok"`
	Running      bool           `json:"running"`
	Trigger      string         `json:"trigger"`
	StartedAt    string         `json:"started_at"`
	FinishedAt   string         `json:"finished_at,omitempty"`
	EarnedCredit float64        `json:"earned_credit"`
	Message      string         `json:"message,omitempty"`
	Results      []TaskResult   `json:"results"`
	Auth         *coreauth.Auth `json:"-"`
}

type AccountTaskStatus struct {
	AuthIndex string       `json:"auth_index"`
	Name      string       `json:"name"`
	Realm     string       `json:"realm"`
	Personal  bool         `json:"personal"`
	CheckedAt string       `json:"checked_at"`
	Checkin   TaskCheckin  `json:"checkin"`
	Activity  TaskActivity `json:"activity"`
	Growth    GrowthStatus `json:"growth"`
	Running   bool         `json:"running"`
	LastRun   *TaskRun     `json:"last_run,omitempty"`
}

type taskSpec struct {
	kind       string
	name       string
	target     int
	reward     float64
	manual     bool
	reason     string
	expertID   string
	expertName string
}

var taskSpecs = map[string]taskSpec{
	"create_canvas":                 {kind: "canvas", name: "创建设计任务", target: 1, reward: 300},
	"template_5":                    {kind: "template", name: "模板创建任务", target: 5, reward: 200},
	"expert_5":                      {kind: "expert", name: "使用专家助手", target: 5, reward: 200},
	"Expert_team_use_3":             {kind: "team", name: "使用专家团队", target: 3, reward: 150},
	"skill_1":                       {kind: "skill", name: "体验技能", target: 1, reward: 100},
	"automation_1":                  {kind: "automation", name: "创建自动化任务", target: 1, reward: 100},
	"playbook_prompt":               {kind: "playbook", name: "灵感案例使用", target: 1, reward: 100},
	"Expert_lighthouse":             {kind: "lighthouse", name: "轻量云专家使用", target: 1, reward: 100},
	"Buddy_App":                     {kind: "buddy", name: "进入 Buddy 应用", target: 1, reward: 100, manual: true, reason: "需要在桌面端实际进入 Buddy 应用"},
	"Buddy_App_QQ":                  {kind: "buddy", name: "企鹅教师助手", target: 1, reward: 100, manual: true, reason: "需要在桌面端实际进入企鹅教师助手"},
	"Hp_Appearance":                 {kind: "skin", name: "应用主题外观", target: 1, reward: 100},
	"chat_5":                        {kind: "chat", name: "发起 5 次对话", target: 5, reward: 100},
	"Model_chat_GLM5.2":             {kind: "glmchat", name: "体验 GLM-5.2", target: 1, reward: 100},
	"black_cat":                     {kind: "cat", name: "夜猫子任务（23:00-08:00）", target: 3, reward: 100},
	"RichMeow_Chat":                 {kind: "richmeow", name: "桌面对话事件链", target: 1, reward: 100},
	"Library_read":                  {kind: "library", name: "浏览资料库", target: 1, reward: 100},
	"first_buddy":                   {kind: "chat", name: "领取一只 Buddy", target: 1, reward: 300},
	"WorkBuddy_Valorant":            {kind: "expert", name: "体验无畏契约拼豆专家", target: 1, reward: 100, expertID: "ex_xQHHD1u5Jkua", expertName: "无畏契约拼豆专家"},
	"workstation_expert":            {kind: "expert", name: "工作台搭建师", target: 1, reward: 100, expertID: "WorkspaceBuilder", expertName: "工作台搭建师"},
	"expert_use_maidehao":           {kind: "expert", name: "中国电商运营专家", target: 1, reward: 100, expertID: "ex_GDl31WahyIH0", expertName: "中国电商运营专家"},
	"expert_use_douyinyin":          {kind: "expert", name: "抖音策略", target: 1, reward: 100, expertID: "ex_FbY5nlWTqDod", expertName: "抖音策略"},
	"expert_use_shangjia_xuanchuan": {kind: "expert", name: "商家宣传页与海报设计", target: 1, reward: 100, expertID: "ex_Weo3x2EZuSDm", expertName: "商家宣传页与海报设计"},
	"expert_use_bowangwang":         {kind: "expert", name: "直播电商教练", target: 1, reward: 100, expertID: "ex_WZY136wV5aMt", expertName: "直播电商教练"},
	"Sequential_Tasks_7":            {kind: "playbook", name: "PC 灵感功能", target: 1, reward: 100},
	"wb_wechat_oa_subscribe_task":   {name: "关注官方公众号", target: 1, reward: 100, manual: true, reason: "需要在微信关注腾讯 WorkBuddy 公众号"},
	"Expert_Philanthropy":           {name: "公益爱心捐赠", target: 1, manual: true, reason: "需要完成真实公益捐赠"},
}

var expertIDPattern = regexp.MustCompile(`(?:ex_[A-Za-z0-9]+|WorkspaceBuilder|ContentCreator)`)
var taskCheckinLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

var expertPool = [][2]string{
	{"ex_PZw8Gu81HfN4", "运维工程师"}, {"ex_ROsDtJbzADFV", "产品经理"},
	{"ex_SMUnl0nJbPix", "UI设计师"}, {"ex_ZTR062oVBOCW", "数据分析师"},
	{"ex_a3sSSFBy8qaC", "后端架构师"}, {"ex_aG1kvKbq8lPx", "文案策划"},
	{"ex_al1vxtUOYQ10", "测试专家"}, {"ex_cZfiyuET9UQP", "安全顾问"},
	{"ex_eggOvQuVP0hq", "算法工程师"}, {"ex_hSwsQjkSKnkX", "前端工程师"},
	{"ex_mMbwwmFA9n9P", "项目管理专家"}, {"ex_uAQE5POfk7Zh", "增长运营专家"},
	{"ex_uZzSAScSy7FZ", "行业研究员"}, {"ex_LHywGrZOtG7G", "数据分析师"},
	{"ex_NX5C8GBciVed", "测试架构师"}, {"ex_DdCsaoq4AtcO", "云端运维专家"},
	{"ex_KzqKQguubrNQ", "内容创作专家"}, {"ex_2cvvUZQhDyeJ", "腾讯轻量云专家"},
}

var teamPool = [][2]string{
	{"CloudOpsTeam", "运维专家团队"}, {"CloudContentTeam", "内容专家团队"},
	{"CloudDevTeam", "研发专家团队"}, {"ProductStrategyTeam", "产品战略团队"},
	{"MarketingCampaignTeam", "营销活动团队"}, {"SalesBattleTeam", "销售作战团队"},
	{"DesignEngineTeam", "设计引擎团队"}, {"HrOperationsTeam", "人力运营团队"},
}

// TaskClient accesses the account's regional activity programme without exposing tokens to the panel.
type TaskClient struct {
	provider string
	service  *Service
	creds    Credentials
	base     string
	billing  string
	now      func() time.Time
}

func NewTaskClient(auth *coreauth.Auth, client *http.Client) *TaskClient {
	provider := auth.Provider
	base := EndpointForAuth(auth)
	billing := taskChinaBillingBase
	if configured, _ := auth.Metadata["billing_url"].(string); strings.TrimSpace(configured) != "" {
		billing = strings.TrimRight(strings.TrimSpace(configured), "/")
	}
	return &TaskClient{provider: provider, service: NewService(provider, client), creds: CredentialsFromAuth(auth), base: base, billing: billing, now: time.Now}
}

func (c *TaskClient) call(ctx context.Context, method, endpoint string, body any, extra http.Header) (ojson.Result, error) {
	var payload []byte
	if body != nil {
		doc := ojson.NewDocString(`{}`)
		switch values := body.(type) {
		case map[string]any:
			for key, value := range values {
				if errSet := doc.Set(key, value); errSet != nil {
					return ojson.Result{}, errSet
				}
			}
		case []any:
			doc = ojson.NewDocString(`[]`)
			for _, value := range values {
				if errSet := doc.Set("-1", value); errSet != nil {
					return ojson.Result{}, errSet
				}
			}
		default:
			return ojson.Result{}, fmt.Errorf("unsupported task payload type %T", body)
		}
		payload = doc.Bytes()
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if errRequest != nil {
		return ojson.Result{}, errRequest
	}
	SignRequest(req, c.provider, c.creds)
	for key, values := range extra {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	res, errDo := c.service.client.Do(req)
	if errDo != nil {
		return ojson.Result{}, fmt.Errorf("workbuddy task request failed: %w", errDo)
	}
	defer func() {
		if errClose := res.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("workbuddy: close task response")
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if errRead != nil {
		return ojson.Result{}, errRead
	}
	root := ojson.ParseBytes(data)
	if !root.IsObject() {
		return ojson.Result{}, fmt.Errorf("workbuddy task returned HTTP %d with an invalid JSON response", res.StatusCode)
	}
	code := int(root.Get("code").Int())
	message := strings.TrimSpace(root.Get("msg").String())
	if res.StatusCode < 200 || res.StatusCode >= 300 || code != 0 {
		if message == "" {
			message = http.StatusText(res.StatusCode)
		}
		return root, &APIError{Code: code, Status: res.StatusCode, Msg: message}
	}
	return root, nil
}

func (c *TaskClient) get(ctx context.Context, path string) (ojson.Result, error) {
	return c.call(ctx, http.MethodGet, c.base+path, nil, nil)
}

func (c *TaskClient) post(ctx context.Context, endpoint string, body any) (ojson.Result, error) {
	return c.call(ctx, http.MethodPost, endpoint, body, nil)
}

func taskText(result ojson.Result, paths ...string) string {
	for _, path := range paths {
		if value := result.Get(path); value.Exists() && strings.TrimSpace(value.String()) != "" {
			return strings.TrimSpace(value.String())
		}
	}
	return ""
}

func taskFloat(result ojson.Result, path string) float64 {
	value := result.Get(path)
	if !value.Exists() {
		return 0
	}
	if value.Type == ojson.Number {
		return value.Float()
	}
	number, errParse := strconv.ParseFloat(value.String(), 64)
	if errParse == nil {
		return number
	}
	return 0
}

func personalTaskAccount(auth *coreauth.Auth) bool {
	creds := CredentialsFromAuth(auth)
	return auth != nil && creds.AccessToken != "" && creds.UID != "" && creds.RefreshToken != "" && creds.EnterpriseID == ""
}

func taskRealm(provider string) string {
	if provider == ProviderAI {
		return "intl"
	}
	return "cn"
}

func taskTime(auth *coreauth.Auth, key string) time.Time {
	value, _ := auth.Metadata[key].(string)
	parsed, errParse := time.Parse(time.RFC3339, value)
	if errParse != nil {
		return time.Time{}
	}
	return parsed
}

func sameTaskDay(value, now time.Time) bool {
	if value.IsZero() {
		return false
	}
	local, current := value.In(now.Location()), now
	return local.Year() == current.Year() && local.YearDay() == current.YearDay()
}

// CheckinCompletedToday reports a confirmed check-in on the current China activity day.
func CheckinCompletedToday(auth *coreauth.Auth, now time.Time) bool {
	return auth.Provider == ProviderCN && sameTaskDay(taskTime(auth, "workbuddy_last_checkin_at"), now.In(taskCheckinLocation))
}

// CheckinDay identifies the billing activity day independently of the server timezone.
func CheckinDay(value time.Time) string {
	return value.In(taskCheckinLocation).Format(time.DateOnly)
}

// Status combines upstream progress with confirmed daily check-in acknowledgements.
func (c *TaskClient) Status(ctx context.Context, auth *coreauth.Auth) (AccountTaskStatus, error) {
	status := AccountTaskStatus{AuthIndex: auth.Index, Name: authFileTaskName(auth), Realm: taskRealm(auth.Provider), Personal: personalTaskAccount(auth), CheckedAt: c.now().UTC().Format(time.RFC3339)}
	status.Checkin.Supported = status.Personal && status.Realm == "cn"
	status.Activity.Supported = status.Personal
	status.Growth.Supported = status.Personal && status.Realm == "cn"
	status.Growth.Tasks = []GrowthTask{}
	if !status.Personal {
		status.Checkin.Message = "仅个人 OAuth 账号支持签到"
		status.Activity.Message = "仅个人 OAuth 账号支持每日活跃"
		status.Growth.Message = "企业账号不参与个人成长任务"
		return status, nil
	}
	if status.Checkin.Supported {
		result, errCheckin := c.post(ctx, c.base+"/v2/billing/meter/checkin-activity-status", map[string]any{})
		if errCheckin != nil {
			status.Checkin.Message = errCheckin.Error()
		} else {
			status.Checkin.CheckedIn = result.Get("data.today_checked_in").Bool()
			status.Checkin.StreakDays = int(result.Get("data.streak_days").Int())
			status.Checkin.DailyCredit = taskFloat(result, "data.daily_credit")
			status.Checkin.TodayCredit = taskFloat(result, "data.today_credit")
		}
		status.Checkin.CheckedIn = status.Checkin.CheckedIn || CheckinCompletedToday(auth, c.now())
	}
	if streak, errStreak := c.get(ctx, "/activity/growth/streak"); errStreak == nil {
		days := int(streak.Get("data.streak.days").Int())
		status.Activity.StreakDays = days
		status.Growth.StreakDays = days
	} else if status.Activity.Message == "" {
		status.Activity.Message = "连登状态查询失败：" + errStreak.Error()
	}
	status.Activity.LastRunAt = storedTaskTime(auth, "workbuddy_last_activity_at")
	status.Activity.Completed = sameTaskDay(taskTime(auth, "workbuddy_last_activity_at"), c.now())
	if status.Realm == "cn" {
		tasks, errTasks := c.GrowthTasks(ctx)
		if errTasks != nil {
			status.Growth.Message = errTasks.Error()
		} else {
			status.Growth.Tasks = tasks
			status.Growth.Total = len(tasks)
			for _, task := range tasks {
				if task.Status == "claimed" {
					status.Growth.Completed++
				}
			}
		}
	} else {
		status.Growth.Message = "国际版使用每日活跃任务"
	}
	return status, nil
}

func authFileTaskName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if strings.TrimSpace(auth.FileName) != "" {
		return auth.FileName
	}
	return auth.ID
}

func storedTaskTime(auth *coreauth.Auth, key string) string {
	value, _ := auth.Metadata[key].(string)
	return value
}

func (c *TaskClient) GrowthTasks(ctx context.Context) ([]GrowthTask, error) {
	root, errGet := c.get(ctx, "/v2/activity/growth/tasks")
	if errGet != nil {
		return nil, errGet
	}
	rawTasks := root.Get("data.tasks")
	if !rawTasks.Exists() {
		return nil, fmt.Errorf("workbuddy growth response is missing data.tasks")
	}
	tasks := make([]GrowthTask, 0)
	for _, raw := range rawTasks.Array() {
		code := raw.Get("task_code").String()
		if code == "" {
			continue
		}
		spec := taskSpecs[code]
		name := taskText(raw, "title")
		if name == "" {
			name = spec.name
		}
		if name == "" {
			name = code
		}
		target := int(raw.Get("progress.target").Int())
		if target <= 0 {
			target = spec.target
		}
		if target <= 0 {
			target = 1
		}
		reward := taskFloat(raw, "reward_credit")
		if reward == 0 {
			reward = spec.reward
		}
		automatable := spec.kind != "" && !spec.manual
		reason := spec.reason
		if spec.kind == "" && !spec.manual {
			jump := raw.Get("jump_url").String()
			if match := expertIDPattern.FindString(jump); match != "" {
				spec = taskSpec{kind: "expert", expertID: match, expertName: name, name: name}
				automatable = true
			}
		}
		if spec.kind == "" && !automatable && reason == "" {
			reason = "该任务不在已验证的自动化清单中"
		}
		state := raw.Get("accept_status").String()
		if state == "" {
			state = "not_accepted"
		}
		tasks = append(tasks, GrowthTask{Code: code, Name: name, Description: taskText(raw, "description", "task_desc"), Status: state, Type: raw.Get("task_type").String(), Current: int(raw.Get("progress.current").Int()), Target: target, RewardCredit: reward, RewardEnergy: taskFloat(raw, "reward_energy"), Automatable: automatable, ManualReason: reason, spec: spec})
	}
	return tasks, nil
}

func checkinState(code int, message string) string {
	pending := []string{"处理中", "请勿重复操作", "系统繁忙", "稍后再试"}
	for _, hint := range pending {
		if strings.Contains(message, hint) {
			return "pending"
		}
	}
	if code == 0 {
		if strings.Contains(message, "失败") {
			return "failed"
		}
		return "checked"
	}
	if code == 10001 && strings.Contains(message, "已签") {
		return "already"
	}
	return "failed"
}

func checkinMessage(result ojson.Result) string {
	if message := taskText(result, "msg"); message != "" {
		return message
	}
	return "ok"
}

func (c *TaskClient) checkin(ctx context.Context, status AccountTaskStatus) TaskResult {
	result := TaskResult{Code: "checkin", Name: "每日签到", OK: true, Status: "completed"}
	if !status.Checkin.Supported {
		result.Status = "unsupported"
		result.Message = status.Checkin.Message
		return result
	}
	if status.Checkin.CheckedIn {
		result.Status = "already"
		result.Message = "今日已签到"
		return result
	}
	response, errCheckin := c.post(ctx, c.billing+"/v2/billing/meter/daily-checkin", map[string]any{})
	code := 0
	message := "请求失败"
	var apiErr *APIError
	if errCheckin != nil {
		code = -1
		if errors.As(errCheckin, &apiErr) {
			if apiErr.Status < http.StatusOK || apiErr.Status >= http.StatusMultipleChoices {
				return failedTaskResult(result, errCheckin.Error())
			}
			code = apiErr.Code
			message = apiErr.Msg
		} else {
			return failedTaskResult(result, errCheckin.Error())
		}
	} else {
		code = int(response.Get("code").Int())
		message = checkinMessage(response)
	}
	state := checkinState(code, message)
	result.Status = state
	result.Message = message
	if state == "already" {
		return result
	}
	if state == "checked" {
		if reward := taskFloat(response, "data.credit"); reward > 0 {
			result.RewardCredit = reward
		} else {
			result.RewardCredit = taskFloat(response, "data.daily_credit")
		}
		return result
	}
	result.OK = false
	return result
}

func failedTaskResult(result TaskResult, message string) TaskResult {
	result.OK = false
	result.Status = "failed"
	result.Message = message
	return result
}

func (c *TaskClient) activity(ctx context.Context, status AccountTaskStatus) TaskResult {
	result := TaskResult{Code: "daily_activity", Name: "每日对话活跃", OK: true, Status: "completed"}
	if !status.Activity.Supported {
		result.Status = "unsupported"
		result.Message = status.Activity.Message
		return result
	}
	if status.Activity.Completed {
		result.Message = "今日活跃已完成"
		return result
	}
	event := c.buildEvent("chat", 0, nil)
	if _, errReport := c.post(ctx, c.base+"/v2/report", event); errReport != nil {
		return failedTaskResult(result, "活跃上报失败："+errReport.Error())
	}
	streak, errStreak := c.get(ctx, "/activity/growth/streak")
	if errStreak != nil {
		result.Message = "活跃上报成功，连登天数查询失败"
		return result
	}
	days := int(streak.Get("data.streak.days").Int())
	result.Current, result.Target = days, days
	result.Message = fmt.Sprintf("活跃上报成功，连续打卡 %d 天", days)
	return result
}

func (c *TaskClient) acceptTasks(ctx context.Context, tasks []GrowthTask) error {
	codes := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task.Status == "not_accepted" && task.Automatable && task.Type != "auto" {
			codes = append(codes, task.Code)
		}
	}
	if len(codes) == 0 {
		return nil
	}
	response, errAccept := c.post(ctx, c.base+"/v2/activity/growth/tasks/accept", map[string]any{"task_codes": codes})
	if errAccept != nil {
		return errAccept
	}
	for _, result := range response.Get("data.results").Array() {
		state := result.Get("status").String()
		if state != "accepted" && state != "already_accepted" {
			return fmt.Errorf("task %s was not accepted: %s", result.Get("task_code").String(), result.Get("message").String())
		}
	}
	return nil
}

func (c *TaskClient) claimTask(ctx context.Context, task GrowthTask) TaskResult {
	result := TaskResult{Code: task.Code, Name: task.Name, Current: task.Current, Target: task.Target, Status: task.Status}
	response, errClaim := c.post(ctx, c.base+"/activity/growth/tasks/"+url.PathEscape(task.Code)+"/claim", map[string]any{})
	if errClaim != nil && c.provider == ProviderCN {
		headers := make(http.Header)
		headers.Set("Origin", taskChinaWebBase)
		headers.Set("Referer", taskChinaWebBase+"/profile/growth-center")
		headers.Set("x-client-platform", "web")
		response, errClaim = c.call(ctx, http.MethodPost, taskChinaWebBase+"/activity/growth/tasks/"+url.PathEscape(task.Code)+"/claim", map[string]any{}, headers)
	}
	if errClaim != nil {
		return failedTaskResult(result, "领奖失败："+errClaim.Error())
	}
	result.OK = true
	result.Status = "claimed"
	result.RewardCredit = taskFloat(response, "data.credit")
	result.Message = "领奖成功"
	return result
}

func (c *TaskClient) progressTask(ctx context.Context, task GrowthTask) TaskResult {
	if task.Status == "claimed" {
		return TaskResult{Code: task.Code, Name: task.Name, OK: true, Status: "claimed", Current: task.Current, Target: task.Target, Message: "奖励已领取"}
	}
	if task.Status == "completed" || (task.Target > 0 && task.Current >= task.Target) {
		return c.claimTask(ctx, task)
	}
	result := TaskResult{Code: task.Code, Name: task.Name, Current: task.Current, Target: task.Target, Status: task.Status}
	if !task.Automatable {
		result.Status = "manual"
		result.Message = task.ManualReason
		return result
	}
	if task.Code == "black_cat" {
		hour := c.now().Hour()
		if hour < 23 && hour >= 8 {
			result.Status = "pending"
			result.Message = "仅 23:00-08:00 可累计夜间任务进度"
			return result
		}
	}
	if task.Status == "not_accepted" && task.Type != "auto" {
		result.Status = "pending"
		result.Message = "任务尚未接取"
		return result
	}

	spec := task.spec
	needed := max(1, task.Target-task.Current)
	for index := 0; index < needed; index++ {
		if errCtx := ctx.Err(); errCtx != nil {
			return failedTaskResult(result, errCtx.Error())
		}
		var events any
		if spec.kind == "richmeow" {
			events = c.desktopChatEvents(index)
		} else {
			events = c.buildEvent(spec.kind, task.Current+index, &spec)
		}
		if _, errReport := c.post(ctx, c.base+"/v2/report", events); errReport != nil {
			return failedTaskResult(result, "事件上报失败："+errReport.Error())
		}
	}
	if errSleep := sleepTaskContext(ctx, 1500*time.Millisecond); errSleep != nil {
		return failedTaskResult(result, errSleep.Error())
	}
	tasks, errFetch := c.GrowthTasks(ctx)
	if errFetch != nil {
		return failedTaskResult(result, "进度复查失败："+errFetch.Error())
	}
	for _, fresh := range tasks {
		if fresh.Code == task.Code {
			result.Current = fresh.Current
			result.Status = fresh.Status
			if fresh.Status == "completed" || fresh.Current >= fresh.Target {
				return c.claimTask(ctx, fresh)
			}
			result.Message = fmt.Sprintf("进度 %d/%d，将在下次巡检继续", fresh.Current, fresh.Target)
			return result
		}
	}
	return failedTaskResult(result, "上报后未找到该任务的最新进度")
}

func sleepTaskContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func taskIDs(creds Credentials, kind string, index int) (string, string, int64) {
	now := time.Now().UnixMilli()
	stem := fmt.Sprintf("wb-task-%d-%s-%d", now, kind, index)
	return stem, stem + "-req", now
}

func (c *TaskClient) buildEvent(kind string, index int, spec *taskSpec) map[string]any {
	cid, rid, now := taskIDs(c.creds, kind, index)
	base := map[string]any{"timestamp": now, "reportDelay": 0, "userId": c.creds.UID, "userNickname": c.creds.Nickname, "conversationId": cid, "requestId": rid}
	switch kind {
	case "canvas":
		base["eventCode"] = "wbx_design_canvas_task_create"
		base["source"], base["isCustomModel"], base["name"], base["inputLength"], base["id"], base["cost"], base["isSuccessful"] = "summon_keyword", false, "", 12, "wbx-canvas-"+strconv.FormatInt(now, 10), 0, true
	case "template":
		base["eventCode"] = "agent_task_created_with_template"
		base["isCustomModel"], base["id"], base["name"] = true, strconv.Itoa(index), "幻灯片"
	case "expert", "team", "lighthouse":
		base["eventCode"] = "expert_actual_use"
		id, name := "ContentCreator", "内容创作专家"
		expertType := "agent"
		if kind == "team" {
			id, name, expertType = teamPool[index%len(teamPool)][0], teamPool[index%len(teamPool)][1], "team"
		} else if kind == "lighthouse" {
			id, name = "ex_2cvvUZQhDyeJ", "腾讯轻量云专家"
		} else {
			id, name = expertPool[index%len(expertPool)][0], expertPool[index%len(expertPool)][1]
		}
		if spec != nil && spec.expertID != "" {
			id, name = spec.expertID, spec.expertName
		}
		base["mode"], base["id"], base["name"], base["expertTitle"], base["type"], base["expertType"], base["source"], base["version"], base["cost"], base["characterCount"], base["messageId"], base["requestModelId"], base["requestModelName"] = "CLOUD", id, name, name, "02-Engineering", expertType, "builtin", "1.0.2", 0, 12, rid, "deepseek-v4-flash", "DeepSeek V4 Flash"
	case "skill":
		base["eventCode"], base["skillId"], base["name"] = "skill_info", "skill_2096525080079265792", "pptx"
	case "automation":
		base["eventCode"], base["name"], base["type"], base["source"], base["modelId"], base["modelIsThinking"], base["prompt"] = "automated_task_create_suc", "每周工作整理", "cron", "manually", "deepseek-v4-flash", false, "每周五自动整理本周工作"
		base["schedule"] = map[string]any{"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"}
	case "library":
		base["eventCode"], base["pageURL"], base["elementId"], base["elementName"], base["os"], base["osVersion"], base["userAgent"] = "web_element_click", "https://www.workbuddy.cn/library", "library_doc_intro_click", "WorkBuddy资料库介绍", "Win32", "10.0", "Mozilla/5.0"
	case "playbook":
		base["eventCode"], base["id"], base["name"], base["type"], base["source"], base["position"] = "playbook_prompt_send", "worker-ledger-freedom-dashboard", "打工人小账本", "document", "discover", 0
		base["promptLength"], base["isOfficial"], base["messageId"], base["skills"], base["skillNames"] = 30, 1, rid, "", ""
	case "skin":
		base["eventCode"], base["action"], base["source"], base["id"], base["vipLevel"], base["series"], base["type"], base["name"] = "appearance_skin_apply", "apply", "settings_close", "theme-tkmw7j", "free", "craft", "unknown", "和平精英激战金沙"
	case "chat", "glmchat", "cat":
		modelID, modelName, mode := "deepseek-v4-flash", "DeepSeek V4 Flash", "craft"
		if kind == "glmchat" || kind == "cat" {
			modelID, modelName = "glm-5.2", "GLM-5.2"
		}
		if kind == "cat" {
			mode = "night"
		}
		base["eventCode"], base["mode"], base["inputLength"], base["requestModelId"], base["requestModelName"], base["isPlan"], base["agentName"], base["agentType"] = "chat_request_send", mode, 12, modelID, modelName, false, "default", "conversation"
	default:
		base["eventCode"] = "heartbeat"
	}
	return base
}

func (c *TaskClient) desktopChatEvents(index int) []any {
	cid, rid, now := taskIDs(c.creds, "richmeow", index)
	messageID := "msg-" + strconv.FormatInt(now, 10)
	return []any{
		map[string]any{"eventCode": "agent_task_created", "timestamp": now, "userId": c.creds.UID, "source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft", "requestModelId": "fast-model", "requestModelName": "fast-model", "conversationId": cid, "messageId": messageID},
		map[string]any{"eventCode": "chat_message_send", "timestamp": now + 20, "userId": c.creds.UID, "messageId": messageID + "-ast", "historyCount": 0, "traceId": rid, "rootRequestId": rid, "parentConversationId": cid, "agentName": "cli", "agentType": "main"},
		map[string]any{"eventCode": "chat_request_send", "timestamp": now + 40, "userId": c.creds.UID, "inputLength": 24, "isPlan": false, "traceId": rid, "rootRequestId": rid, "parentConversationId": cid, "conversationId": cid, "messageId": messageID, "agentName": "cli", "agentType": "main"},
		map[string]any{"eventCode": "chat_message_response", "timestamp": now + 60, "userId": c.creds.UID, "messageId": messageID + "-resp", "conversationId": cid, "requestId": rid, "agentName": "cli"},
		map[string]any{"eventCode": "chat_request_response", "timestamp": now + 80, "userId": c.creds.UID, "conversationId": cid, "requestId": rid, "requestModelId": "fast-model", "requestModelName": "fast-model", "toolCallCount": 0, "inputToken": 6, "outputToken": 12, "duration": 50, "agentName": "cli"},
		map[string]any{"eventCode": "agent_task_completed", "timestamp": now + 100, "userId": c.creds.UID, "conversationId": cid, "messageId": messageID, "status": "success"},
	}
}

// Run performs explicit or configured account actions and returns per-task outcomes.
func (c *TaskClient) Run(ctx context.Context, auth *coreauth.Auth, checkin, growth bool, trigger string) (TaskRun, error) {
	started := c.now()
	run := TaskRun{OK: true, Trigger: trigger, StartedAt: started.UTC().Format(time.RFC3339), Results: []TaskResult{}, Auth: auth.Clone()}
	status, errStatus := c.Status(ctx, auth)
	if errStatus != nil {
		return run, errStatus
	}
	if checkin && status.Realm == "cn" && status.Personal {
		result := c.checkin(ctx, status)
		run.Results = append(run.Results, result)
		if result.OK {
			if !CheckinCompletedToday(auth, started) {
				run.Auth.Metadata["workbuddy_last_checkin_at"] = started.UTC().Format(time.RFC3339)
			}
			status.Checkin.CheckedIn = true
		}
	}
	if growth && status.Personal {
		if status.Realm == "cn" && status.Growth.Message != "" {
			run.Results = append(run.Results, failedTaskResult(TaskResult{Code: "growth", Name: "成长任务"}, status.Growth.Message))
		}
		if status.Realm == "cn" && status.Growth.Message == "" {
			tasks, errTasks := c.GrowthTasks(ctx)
			if errTasks != nil {
				run.Results = append(run.Results, failedTaskResult(TaskResult{Code: "growth", Name: "成长任务"}, errTasks.Error()))
			} else {
				for _, task := range tasks {
					if task.Type == "auto" {
						result := c.progressTask(ctx, task)
						run.Results = append(run.Results, result)
					}
				}
				if errAccept := c.acceptTasks(ctx, tasks); errAccept != nil {
					run.Results = append(run.Results, failedTaskResult(TaskResult{Code: "accept", Name: "接取成长任务"}, errAccept.Error()))
				} else if len(tasks) > 0 {
					tasks, errTasks = c.GrowthTasks(ctx)
					if errTasks != nil {
						run.Results = append(run.Results, failedTaskResult(TaskResult{Code: "growth", Name: "成长任务进度"}, errTasks.Error()))
						tasks = nil
					}
				}
				for _, task := range tasks {
					if task.Type == "auto" {
						continue
					}
					result := c.progressTask(ctx, task)
					run.Results = append(run.Results, result)
				}
			}
		}
		result := c.activity(ctx, status)
		run.Results = append(run.Results, result)
		if result.OK && !status.Activity.Completed {
			run.Auth.Metadata["workbuddy_last_activity_at"] = c.now().UTC().Format(time.RFC3339)
		}
	}
	for _, result := range run.Results {
		run.OK = run.OK && result.OK
		run.EarnedCredit += result.RewardCredit
	}
	run.FinishedAt = c.now().UTC().Format(time.RFC3339)
	if len(run.Results) == 0 {
		run.Message = "当前账号没有可执行的签到或成长任务"
	} else if !run.OK {
		run.Message = "部分任务未成功，请查看任务明细"
	} else {
		run.Message = fmt.Sprintf("执行完成，本次新增 %.0f 积分", run.EarnedCredit)
	}
	return run, nil
}

package workbuddy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func taskTestAuth(provider, endpoint string) *coreauth.Auth {
	record := NewAuthRecord(provider, Credentials{AccessToken: "access", RefreshToken: "refresh", UID: "uid", Nickname: "Tester", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	record.Index = "test-index"
	record.Metadata["base_url"] = endpoint
	record.Metadata["billing_url"] = endpoint
	return record
}

func TestTaskStatusAndCheckinReportDetailedRewards(t *testing.T) {
	var mu sync.Mutex
	checked := false
	var dailyCredit float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("X-User-Id") != "uid" {
			t.Error("task request did not use the selected account")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":` + strconv.FormatBool(checked) + `,"streak_days":7,"daily_credit":30,"today_credit":` + strconv.FormatFloat(dailyCredit, 'f', -1, 64) + `}}`))
		case "/v2/billing/meter/daily-checkin":
			checked = true
			dailyCredit = 30
			_, _ = w.Write([]byte(`{"code":0,"msg":"签到成功","data":{"credit":30}}`))
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":7}}}`))
		case "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"chat_5","title":"发起 5 次对话","accept_status":"claimed","progress":{"current":5,"target":5},"reward_credit":100},{"task_code":"wb_wechat_oa_subscribe_task","title":"关注官方公众号","accept_status":"not_accepted","progress":{"current":0,"target":1},"reward_credit":100}]}}`))
		default:
			t.Errorf("unexpected task path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	auth := taskTestAuth(ProviderCN, server.URL)
	client := NewTaskClient(auth, server.Client())
	status, errStatus := client.Status(t.Context(), auth)
	if errStatus != nil || status.Checkin.CheckedIn || status.Growth.Completed != 1 || status.Growth.Total != 2 {
		t.Fatalf("unexpected initial task status: %+v, %v", status, errStatus)
	}
	run, errRun := client.Run(t.Context(), auth, true, false, "manual")
	if errRun != nil || !run.OK || run.EarnedCredit != 30 || len(run.Results) != 1 || run.Results[0].Status != "checked" {
		t.Fatalf("checkin result lost reward or success: %+v, %v", run, errRun)
	}
	status, errStatus = client.Status(t.Context(), auth)
	if errStatus != nil || !status.Checkin.CheckedIn || status.Checkin.TodayCredit != 30 {
		t.Fatalf("refreshed checkin status is not upstream-backed: %+v, %v", status, errStatus)
	}
	manual := status.Growth.Tasks[1]
	if manual.Automatable || manual.ManualReason == "" {
		t.Fatal("manual-only task was presented as automatable")
	}
	again, errAgain := client.Run(t.Context(), run.Auth, true, false, "manual")
	if errAgain != nil || !again.OK || again.EarnedCredit != 0 || again.Results[0].RewardCredit != 0 {
		t.Fatalf("previous checkin credit was counted again: %+v, %v", again, errAgain)
	}
}

func TestCheckinDoesNotTreatPendingAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":false}}`))
		case "/v2/billing/meter/daily-checkin":
			_, _ = w.Write([]byte(`{"code":0,"msg":"请求处理中，请勿重复操作"}`))
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":0}}}`))
		case "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
		}
	}))
	defer server.Close()
	client := NewTaskClient(taskTestAuth(ProviderCN, server.URL), server.Client())
	run, errRun := client.Run(t.Context(), taskTestAuth(ProviderCN, server.URL), true, false, "auto")
	if errRun != nil || run.OK || len(run.Results) != 1 || run.Results[0].Status != "pending" {
		t.Fatalf("pending checkin was marked as successful: %+v, %v", run, errRun)
	}
}

func TestGrowthTaskRunsAndClaimsWithReportedCredit(t *testing.T) {
	accepted := false
	progress := 0
	var event ojson.Result
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		case "/v2/activity/growth/tasks":
			status := "not_accepted"
			if accepted {
				status = "accepted"
			}
			if progress > 0 {
				status = "completed"
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"chat_5","title":"发起 5 次对话","accept_status":"` + status + `","progress":{"current":` + strconv.Itoa(progress) + `,"target":5},"reward_credit":100}]}}`))
		case "/v2/activity/growth/tasks/accept":
			accepted = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[{"task_code":"chat_5","status":"accepted"}]}}`))
		case "/v2/report":
			body, _ := io.ReadAll(r.Body)
			event = ojson.ParseBytes(body)
			progress = 5
			_, _ = w.Write([]byte(`{"code":0}`))
		case "/activity/growth/tasks/chat_5/claim":
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":125,"energy":3}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	auth := taskTestAuth(ProviderCN, server.URL)
	run, errRun := NewTaskClient(auth, server.Client()).Run(t.Context(), auth, false, true, "manual")
	if errRun != nil || !run.OK || run.EarnedCredit != 125 {
		t.Fatalf("growth execution did not return actual claimed credit: %+v, %v", run, errRun)
	}
	if event.Get("eventCode").String() != "chat_request_send" || event.Get("userId").String() != "uid" {
		t.Fatalf("unexpected growth event: %s", event.Raw)
	}
	foundClaim := false
	for _, result := range run.Results {
		if result.Code == "chat_5" && result.Status == "claimed" && result.RewardCredit == 125 {
			foundClaim = true
		}
	}
	if !foundClaim {
		t.Fatalf("per-task claim result is missing: %+v", run.Results)
	}
	if run.Auth.Metadata["workbuddy_last_activity_at"] == "" {
		t.Fatal("successful activity timestamp was not prepared for persistence")
	}
}

func TestInternationalDailyActivityIsIdempotentPerDay(t *testing.T) {
	var reports int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":9}}}`))
		case "/v2/report":
			reports++
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			t.Errorf("international account called China-only task path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	auth := taskTestAuth(ProviderAI, server.URL)
	client := NewTaskClient(auth, server.Client())
	run, errRun := client.Run(t.Context(), auth, true, true, "auto")
	if errRun != nil || !run.OK || reports != 1 || len(run.Results) != 1 || run.Results[0].Code != "daily_activity" || !strings.Contains(run.Results[0].Message, "9") {
		t.Fatalf("international activity result = %+v, %v", run, errRun)
	}
	again, errAgain := client.Run(t.Context(), run.Auth, true, true, "auto")
	if errAgain != nil || reports != 1 || !again.Results[0].OK || again.Results[0].Message != "今日活跃已完成" {
		t.Fatalf("activity repeated on the same day: %+v, %v", again, errAgain)
	}
}

func TestTaskClientRejectsInvalidJSONAndEnterpriseAccounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>bad gateway</html>`)) }))
	defer server.Close()
	auth := taskTestAuth(ProviderCN, server.URL)
	client := NewTaskClient(auth, server.Client())
	if _, err := client.GrowthTasks(t.Context()); err == nil {
		t.Fatal("invalid upstream response was accepted")
	}
	auth.Metadata["enterprise_id"] = "enterprise"
	status, err := client.Status(t.Context(), auth)
	if err != nil || status.Personal || status.Checkin.Supported || status.Growth.Supported {
		t.Fatalf("enterprise account received personal tasks: %+v, %v", status, err)
	}
}

func TestTaskRunReportsFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		failPath string
		code     string
	}{
		{"activity", ProviderAI, "/v2/report", "daily_activity"},
		{"growth query", ProviderCN, "/v2/activity/growth/tasks", "growth"},
		{"automatic task", ProviderCN, "/v2/report", "chat_5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.failPath {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"code":503,"msg":"unavailable"}`))
					return
				}
				switch r.URL.Path {
				case "/v2/billing/meter/checkin-activity-status":
					_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
				case "/activity/growth/streak":
					_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":0}}}`))
				case "/v2/activity/growth/tasks":
					_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"chat_5","task_type":"auto","accept_status":"accepted","progress":{"current":0,"target":5}}]}}`))
				case "/v2/report":
					_, _ = w.Write([]byte(`{"code":0}`))
				default:
					t.Errorf("unexpected task path: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			auth := taskTestAuth(tc.provider, server.URL)
			run, errRun := NewTaskClient(auth, server.Client()).Run(t.Context(), auth, false, true, "manual")
			if errRun != nil || run.OK || run.EarnedCredit != 0 {
				t.Fatalf("failed task was reported as successful: %+v, %v", run, errRun)
			}
			for _, result := range run.Results {
				if result.Code == tc.code && !result.OK && result.Status == "failed" {
					return
				}
			}
			t.Fatalf("failure details missing for %s: %+v", tc.code, run.Results)
		})
	}
}

func TestTaskRequestPreservesObjectAndArrayBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
	}{
		{"empty object", map[string]any{}},
		{"object", map[string]any{"eventCode": "chat_request_send", "detail": map[string]any{"text": "a\"b"}}},
		{"array", []any{map[string]any{"eventCode": "agent_task_created"}, map[string]any{"eventCode": "agent_task_completed"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
				}
				root := ojson.ParseBytes(body)
				if tc.name == "array" {
					if !root.IsArray() || len(root.Array()) != 2 || root.Get("1.eventCode").String() != "agent_task_completed" {
						t.Errorf("unexpected array body: %s", body)
					}
				} else if !root.IsObject() || root.Get("payload").Exists() {
					t.Errorf("unexpected object body: %s", body)
				} else if tc.name == "object" && root.Get("detail.text").String() != "a\"b" {
					t.Errorf("nested value was changed: %s", body)
				}
				_, _ = w.Write([]byte(`{"code":0}`))
			}))
			defer server.Close()
			client := NewTaskClient(taskTestAuth(ProviderCN, server.URL), server.Client())
			if _, errPost := client.post(t.Context(), server.URL, tc.body); errPost != nil {
				t.Fatal(errPost)
			}
		})
	}
}

func TestGrowthTasksPreserveExpertMappingAndManualRestrictions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"new_expert","title":"New expert","jump_url":"/expert/ex_TEST123","progress":{"current":0,"target":1}},{"task_code":"Expert_Philanthropy","jump_url":"/expert/ex_TEST123","progress":{"current":0,"target":1}}]}}`))
	}))
	defer server.Close()
	client := NewTaskClient(taskTestAuth(ProviderCN, server.URL), server.Client())
	tasks, errTasks := client.GrowthTasks(t.Context())
	if errTasks != nil || len(tasks) != 2 {
		t.Fatalf("growth tasks: %+v, %v", tasks, errTasks)
	}
	event := client.buildEvent(tasks[0].spec.kind, 0, &tasks[0].spec)
	if !tasks[0].Automatable || event["eventCode"] != "expert_actual_use" || event["id"] != "ex_TEST123" {
		t.Fatalf("dynamic expert mapping was lost: %+v", event)
	}
	if tasks[1].Automatable || tasks[1].ManualReason != taskSpecs["Expert_Philanthropy"].reason {
		t.Fatal("manual-only task was converted into an automated expert task")
	}
}

func TestCheckinRejectsHTTPFailureAndDoesNotRecountAlreadyReward(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"HTTP failure", http.StatusServiceUnavailable, `{"code":0,"msg":"unavailable"}`, false},
		{"already checked", http.StatusOK, `{"code":10001,"msg":"今日已签到","data":{"credit":30}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := NewTaskClient(taskTestAuth(ProviderCN, server.URL), server.Client())
			result := client.checkin(t.Context(), AccountTaskStatus{Checkin: TaskCheckin{Supported: true}})
			if result.OK != tc.ok || result.RewardCredit != 0 {
				t.Fatalf("unexpected checkin result: %+v", result)
			}
		})
	}
}

func TestConfirmedCheckinUsesChinaActivityDay(t *testing.T) {
	auth := taskTestAuth(ProviderCN, "http://tasks.invalid")
	auth.Metadata["workbuddy_last_checkin_at"] = "2026-11-05T15:59:00Z"
	beforeMidnight := time.Date(2026, 11, 5, 15, 59, 59, 0, time.UTC)
	if !CheckinCompletedToday(auth, beforeMidnight) || CheckinCompletedToday(auth, beforeMidnight.Add(time.Second)) {
		t.Fatal("checkin did not reset at midnight in China")
	}
	if CheckinDay(beforeMidnight) == CheckinDay(beforeMidnight.Add(time.Second)) {
		t.Fatal("cached checkin day did not change at midnight")
	}
}

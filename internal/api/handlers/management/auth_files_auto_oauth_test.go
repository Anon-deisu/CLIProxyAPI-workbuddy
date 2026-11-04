package management

import (
	"testing"
	"time"
)

func TestCodexAutoOAuthJobProgressIsUpdatedWithoutSecrets(t *testing.T) {
	h := &Handler{autoOAuthJobs: make(map[string]*codexAutoOAuthJob)}
	h.autoOAuthJobs["job-1"] = &codexAutoOAuthJob{
		ID:        "job-1",
		Status:    "running",
		CreatedAt: time.Now(),
		Accounts: []codexAutoOAuthResult{{
			Email:    "one@example.test",
			Status:   "queued",
			Step:     "等待调度",
			Progress: 0,
		}},
	}
	h.updateCodexAutoOAuthAccount("job-1", 0, func(result *codexAutoOAuthResult) {
		result.Status = "running"
		result.Step = "填写 2FA 验证码"
		result.Progress = 58
	})

	h.autoOAuthJobsMu.Lock()
	job := cloneCodexAutoOAuthJob(h.autoOAuthJobs["job-1"])
	h.autoOAuthJobsMu.Unlock()
	if job.Accounts[0].Progress != 58 || job.Accounts[0].Step != "填写 2FA 验证码" {
		t.Fatalf("progress = %#v, want step and progress update", job.Accounts[0])
	}
	if job.Accounts[0].Email != "one@example.test" {
		t.Fatal("account identity was lost during progress update")
	}
}

func TestCodexAutoOAuthJobPayloadCountsSuccessfulAccounts(t *testing.T) {
	job := &codexAutoOAuthJob{
		ID:     "job-1",
		Status: "completed",
		Accounts: []codexAutoOAuthResult{
			{Email: "one@example.test", Status: "success", Progress: 100},
			{Email: "two@example.test", Status: "failed", Progress: 100, Error: "需要人工处理"},
		},
	}
	payload := codexAutoOAuthJobPayload(job)
	if payload["success"] != 1 || payload["total"] != 2 || payload["status"] != "completed" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestCodexAutoOAuthFailedAccountKeepsActualPhase(t *testing.T) {
	current := codexAutoOAuthResult{Email: "one@example.test", Status: "running", Step: "等待登录页安全验证", Progress: 20}
	failed := codexAutoOAuthResult{Email: current.Email, Status: "failed", Error: "需要人工处理"}
	finishCodexAutoOAuthAccount(&current, failed)
	if current.Progress != 20 || current.Step != "等待登录页安全验证" || current.Status != "failed" {
		t.Fatalf("failure must not fabricate completed steps: %#v", current)
	}
	finishCodexAutoOAuthAccount(&current, codexAutoOAuthResult{Email: current.Email, Status: "success"})
	if current.Progress != 100 || current.Step != "已完成" {
		t.Fatal("success should finish at 100 percent")
	}
}

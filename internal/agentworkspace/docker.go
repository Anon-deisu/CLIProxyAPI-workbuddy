package agentworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const defaultSandboxImage = "node:22-bookworm"

// DockerRunner reuses one isolated container per conversation workspace.
type DockerRunner struct {
	image  string
	mu     sync.Mutex
	active map[string]bool
}

// NewDockerRunner creates the standard no-network workspace-only sandbox runner.
func NewDockerRunner(image string) *DockerRunner {
	if strings.TrimSpace(image) == "" {
		image = defaultSandboxImage
	}
	return &DockerRunner{image: image, active: make(map[string]bool)}
}

// Status reports whether the Docker CLI can communicate with a daemon.
func (r *DockerRunner) Status(ctx context.Context) SandboxStatus {
	image := defaultSandboxImage
	if r != nil && strings.TrimSpace(r.image) != "" {
		image = r.image
	}
	docker, errLookPath := exec.LookPath("docker")
	if errLookPath != nil {
		return SandboxStatus{Image: image, Message: "Docker CLI is not installed"}
	}
	command := exec.CommandContext(ctx, docker, "version", "--format", "{{.Server.Version}}")
	output, errRun := command.Output()
	if errRun != nil {
		return SandboxStatus{Image: image, Message: "Docker daemon is unavailable to the CPA service"}
	}
	if errImage := exec.CommandContext(ctx, docker, "image", "inspect", image).Run(); errImage != nil {
		return SandboxStatus{Image: image, Message: "Sandbox image is not installed; an administrator must pull the configured image"}
	}
	return SandboxStatus{Available: true, Image: image, Message: strings.TrimSpace(string(output))}
}

// Run executes in the conversation container with no network and only its workspace mounted.
func (r *DockerRunner) Run(ctx context.Context, workspace, commandText string) (CommandResult, error) {
	if r == nil {
		return CommandResult{}, fmt.Errorf("Docker sandbox is unavailable")
	}
	docker, errLookPath := exec.LookPath("docker")
	if errLookPath != nil {
		return CommandResult{}, fmt.Errorf("Docker CLI is not installed")
	}
	absWorkspace, errAbs := filepath.Abs(workspace)
	if errAbs != nil {
		return CommandResult{}, fmt.Errorf("resolve sandbox workspace: %w", errAbs)
	}
	name := sandboxName(absWorkspace)
	if errEnsure := r.ensure(ctx, docker, name, absWorkspace); errEnsure != nil {
		return CommandResult{}, errEnsure
	}
	output := &limitedOutput{limit: maxToolOutputBytes}
	command := exec.CommandContext(ctx, docker, "exec", "--workdir", "/workspace", name, "/bin/sh", "-lc", commandText)
	command.Stdout = output
	command.Stderr = output
	errRun := command.Run()
	if errors.Is(errRun, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		if errRemove := r.Remove(workspace); errRemove != nil {
			return CommandResult{}, fmt.Errorf("cancel sandbox: %w", errRemove)
		}
		return CommandResult{}, context.Canceled
	}
	result := CommandResult{Output: output.String()}
	if output.truncated {
		result.Output += "\n[output truncated]"
	}
	if errRun == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(errRun, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	return CommandResult{}, fmt.Errorf("start Docker sandbox: %w", errRun)
}

func dockerRunArgs(name, workspace, image, commandText string) []string {
	if strings.TrimSpace(image) == "" {
		image = defaultSandboxImage
	}
	var mount bytes.Buffer
	writer := csv.NewWriter(&mount)
	_ = writer.Write([]string{"type=bind", "src=" + workspace, "dst=/workspace"})
	writer.Flush()
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 {
		uid = 1000
	}
	if gid < 0 {
		gid = 1000
	}
	return []string{
		"run", "--detach", "--init", "--name", name,
		"--label", "cpa.agent-workspace=" + name,
		"--pull", "never",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
		"--network", "none",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--pids-limit", "128",
		"--memory", "512m",
		"--cpus", "1",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--mount", strings.TrimSpace(mount.String()),
		"--workdir", "/workspace",
		"--env", "HOME=/workspace",
		"--env", "npm_config_cache=/workspace/.cache/npm",
		"--entrypoint", "/bin/sh",
		image,
		"-lc", commandText,
	}
}

func sandboxName(workspace string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(workspace)))
	return "cpa-agent-" + hex.EncodeToString(digest[:12])
}

func (r *DockerRunner) ensure(ctx context.Context, docker, name, workspace string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[name] {
		return nil
	}
	inspect := exec.CommandContext(ctx, docker, "inspect", name)
	raw, errInspect := inspect.Output()
	if errInspect == nil {
		var containers []struct {
			Config struct{ Labels map[string]string }
			State  struct{ Running bool }
		}
		if errDecode := json.Unmarshal(raw, &containers); errDecode != nil || len(containers) != 1 || containers[0].Config.Labels["cpa.agent-workspace"] != name {
			return fmt.Errorf("sandbox container ownership is invalid")
		}
		if !containers[0].State.Running {
			if errStart := exec.CommandContext(ctx, docker, "start", name).Run(); errStart != nil {
				return fmt.Errorf("restart Docker sandbox: %w", errStart)
			}
		}
		r.active[name] = true
		return nil
	}
	create := exec.CommandContext(ctx, docker, dockerRunArgs(name, workspace, r.image, "while true; do sleep 3600; done")...)
	output := &limitedOutput{limit: maxToolOutputBytes}
	create.Stdout, create.Stderr = output, output
	if errCreate := create.Run(); errCreate != nil {
		_ = exec.Command(docker, "rm", "-f", name).Run()
		return fmt.Errorf("create Docker sandbox: %s", strings.TrimSpace(output.String()))
	}
	r.active[name] = true
	return nil
}

// Remove forcibly stops and removes only this workspace's named container.
func (r *DockerRunner) Remove(workspace string) error {
	docker, errLookPath := exec.LookPath("docker")
	if errLookPath != nil {
		return nil
	}
	absWorkspace, errAbs := filepath.Abs(workspace)
	if errAbs != nil {
		return errAbs
	}
	name := sandboxName(absWorkspace)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active[name] {
		output, errList := exec.Command(docker, "ps", "-aq", "--filter", "name=^/"+name+"$", "--filter", "label=cpa.agent-workspace="+name).Output()
		if errList != nil {
			return fmt.Errorf("check Docker sandbox cleanup: %w", errList)
		}
		if len(bytes.TrimSpace(output)) == 0 {
			return nil
		}
	}
	if output, errRemove := exec.Command(docker, "rm", "-f", name).CombinedOutput(); errRemove != nil {
		return fmt.Errorf("remove Docker sandbox: %s", strings.TrimSpace(string(output)))
	}
	delete(r.active, name)
	return nil
}

type limitedOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = b.buffer.Write(data[:remaining])
		b.truncated = true
		return len(data), nil
	}
	_, _ = b.buffer.Write(data)
	return len(data), nil
}

func (b *limitedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

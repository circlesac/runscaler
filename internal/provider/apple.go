package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

// AppleContainerProvider starts one Linux VM per job using Apple's signed CLI.
// Its Docker daemon and container-job mounts stay inside that job VM.
type AppleContainerProvider struct {
	command      CommandRunner
	image        string
	owner        string
	cpu          int
	memory       int
	readyTimeout time.Duration
	poll         time.Duration
}

type AppleInstance struct {
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func NewAppleContainerProvider(ss config.ScaleSetConfig) *AppleContainerProvider {
	return &AppleContainerProvider{command: execCommandRunner{}, image: ss.RunnerImage, owner: ss.ScaleSetName, cpu: ss.AppleContainer.CPU, memory: ss.AppleContainer.Memory, readyTimeout: 5 * time.Minute, poll: 500 * time.Millisecond}
}

func (p *AppleContainerProvider) list(ctx context.Context) ([]AppleInstance, error) {
	out, err := p.command.Run(ctx, "container", "list", "--all", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("apple container inventory unavailable: %w", err)
	}
	var items []AppleInstance
	if err = json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("invalid apple container inventory: %w", err)
	}
	if items == nil {
		return nil, fmt.Errorf("apple container inventory must be an array")
	}
	return items, nil
}

func (p *AppleContainerProvider) StartInstance(ctx context.Context, name, jit string) (string, error) {
	if jit == "" || strings.ContainsAny(jit, "\r\n") {
		return "", fmt.Errorf("invalid JIT configuration")
	}
	items, err := p.list(ctx)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Configuration.ID == name {
			return "", fmt.Errorf("apple container name already exists")
		}
	}
	secret, err := os.CreateTemp("", "runscaler-apple-jit-*")
	if err != nil {
		return "", err
	}
	path := secret.Name()
	defer os.Remove(path)
	if _, err = secret.WriteString("ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit + "\n"); err != nil {
		secret.Close()
		return "", err
	}
	if err = secret.Close(); err != nil {
		return "", err
	}
	args := []string{"run", "--detach", "--name", name, "--cpus", strconv.Itoa(p.cpu), "--memory", strconv.Itoa(p.memory) + "M", "--user", "root", "--cap-add", "ALL", "--read-only-path", "NONE", "--masked-path", "NONE", "--rosetta", "--env-file", path, "--label", "io.github.runscaler.apple-owner=" + p.owner, "--entrypoint", "/usr/local/bin/apple-runner", p.image}
	if _, err = p.command.Run(ctx, "container", args...); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cleanupErr := p.RemoveInstance(cleanup, name)
		if cleanupErr != nil {
			return "", fmt.Errorf("apple container start failed; cleanup pending: %w", cleanupErr)
		}
		return "", fmt.Errorf("apple container start failed: %w", err)
	}
	return name, nil
}

func (p *AppleContainerProvider) RemoveInstance(ctx context.Context, id string) error {
	items, err := p.list(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Configuration.ID != id {
			continue
		}
		if item.Configuration.Labels["io.github.runscaler.apple-owner"] != p.owner {
			return fmt.Errorf("refusing to delete a foreign apple container")
		}
		if item.Status.State != "stopped" {
			if _, err = p.command.Run(ctx, "container", "stop", "--time", "30", id); err != nil {
				return err
			}
		}
		_, err = p.command.Run(ctx, "container", "delete", id)
		return err
	}
	return nil
}

func (p *AppleContainerProvider) Reconcile(ctx context.Context) error {
	items, err := p.list(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Configuration.Labels["io.github.runscaler.apple-owner"] == p.owner {
			if err = p.RemoveInstance(ctx, item.Configuration.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *AppleContainerProvider) WaitInstance(ctx context.Context, id string) error {
	for {
		items, err := p.list(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, item := range items {
			if item.Configuration.ID == id {
				found = true
				if item.Status.State == "stopped" {
					return nil
				}
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.poll):
		}
	}
}

func (p *AppleContainerProvider) WaitReady(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, p.readyTimeout)
	defer cancel()
	for {
		logs, err := p.command.Run(ctx, "container", "logs", id)
		if err == nil && (strings.Contains(string(logs), "Listening for Jobs") || strings.Contains(string(logs), "Running job:")) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrReadinessTimeout
		case <-time.After(p.poll):
		}
	}
}

func (p *AppleContainerProvider) Shutdown(context.Context) {}

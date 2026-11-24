package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/ysya/runscaler/internal/config"
)

// WindowsUTMProvider clones one golden UTM VM per job. A small ISO containing
// the single-use JIT config is attached by PrepareScript before boot. The
// guest startup task reads that ISO, starts the runner, and powers itself off.
type WindowsUTMProvider struct {
	ss      config.ScaleSetConfig
	log     *slog.Logger
	cmd     CommandRunner
	mu      sync.Mutex
	ports   map[string]int
	jitISOs map[string]string
}

func NewWindowsUTMProvider(ss config.ScaleSetConfig, logger *slog.Logger) *WindowsUTMProvider {
	return &WindowsUTMProvider{ss: ss, log: logger, cmd: execCommandRunner{}, ports: make(map[string]int), jitISOs: make(map[string]string)}
}

func (p *WindowsUTMProvider) allocatePort(name string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if port, ok := p.ports[name]; ok {
		return port, nil
	}
	used := map[int]bool{}
	for _, port := range p.ports {
		used[port] = true
	}
	minPort, maxPort := p.ss.WindowsUTM.PortMin, p.ss.WindowsUTM.PortMax
	if fixedPort := p.ss.WindowsUTM.Port; fixedPort != 0 {
		minPort, maxPort = fixedPort, fixedPort
	}
	for port := minPort; port <= maxPort; port++ {
		if !used[port] {
			p.ports[name] = port
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free Windows UTM SSH port in %d-%d", minPort, maxPort)
}
func (p *WindowsUTMProvider) releasePort(name string) {
	p.mu.Lock()
	delete(p.ports, name)
	p.mu.Unlock()
}

func (p *WindowsUTMProvider) createJITISO(ctx context.Context, name, jit string) (string, error) {
	dir, err := os.MkdirTemp("", "runscaler-jit-")
	if err != nil {
		return "", err
	}
	media := filepath.Join(dir, "media")
	if err := os.Mkdir(media, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	identity := filepath.Join(dir, "ssh-key")
	if _, err := p.cmd.Run(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", identity); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create Windows job SSH key: %w", err)
	}
	publicKey, err := os.ReadFile(identity + ".pub")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	seed, err := json.Marshal(map[string]any{
		"username":            p.ss.WindowsUTM.User,
		"password":            "Aa1!" + rand.Text(),
		"ssh_authorized_keys": []string{strings.TrimSpace(string(publicKey))},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(media, "windows-seed.json"), seed, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(media, "jitconfig.txt"), []byte(jit+"\r\n"), 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	iso := filepath.Join(dir, name+".iso")
	if _, err := p.cmd.Run(ctx, "mkisofs", "-quiet", "-J", "-R", "-V", "MRJIT", "-o", iso, media); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create JIT ISO: %w", err)
	}
	return dir, nil
}

func (p *WindowsUTMProvider) StartInstance(ctx context.Context, name, jit string) (string, error) {
	if p.ss.WindowsUTM.PrepareScript == "" {
		return "", errors.New("windows-utm requires prepare-script to attach JIT ISO")
	}
	port, err := p.allocatePort(name)
	if err != nil {
		return "", err
	}
	jitDir, err := p.createJITISO(ctx, name, jit)
	if err != nil {
		p.releasePort(name)
		return "", err
	}
	p.mu.Lock()
	p.jitISOs[name] = jitDir
	p.mu.Unlock()
	rollback := func(cause error) (string, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return "", errors.Join(cause, p.RemoveInstance(cleanupCtx, name))
	}
	if _, err = p.cmd.Run(ctx, "utmctl", "clone", p.ss.WindowsUTM.Template, "--hide", "--name", name); err != nil {
		return rollback(fmt.Errorf("clone Windows UTM VM: %w", err))
	}
	if _, err = p.cmd.Run(ctx, p.ss.WindowsUTM.PrepareScript, name, strconv.Itoa(port), filepath.Join(jitDir, name+".iso"), "--documents-dir", p.ss.WindowsUTM.DocumentsDir); err != nil {
		return rollback(fmt.Errorf("prepare Windows UTM VM: %w", err))
	}
	if _, err = p.cmd.Run(ctx, "utmctl", "start", name, "--hide"); err != nil {
		return rollback(fmt.Errorf("start Windows UTM VM: %w", err))
	}
	return name, nil
}
func encodePowerShell(script string) []byte {
	codeUnits := utf16.Encode([]rune(script))
	encoded := make([]byte, 0, len(codeUnits)*2)
	for _, unit := range codeUnits {
		encoded = binary.LittleEndian.AppendUint16(encoded, unit)
	}
	return encoded
}

func (p *WindowsUTMProvider) WaitReady(ctx context.Context, id string) error {
	p.mu.Lock()
	dir, port := p.jitISOs[id], p.ports[id]
	p.mu.Unlock()
	if dir == "" || port == 0 {
		return fmt.Errorf("windows-utm instance %q is not tracked", id)
	}
	const probe = `$log = Join-Path $env:ProgramData 'Runscaler/runner.log'; if (Test-Path $log) { if (Select-String -LiteralPath $log -SimpleMatch 'Listening for Jobs' -Quiet) { exit 0 } }; exit 1`
	encoded := base64.StdEncoding.EncodeToString(encodePowerShell(probe))
	ready, cancel := context.WithTimeout(ctx, p.ss.WindowsUTM.ReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		_, err := p.cmd.Run(ready, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-o", "IdentitiesOnly=yes",
			"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile="+filepath.Join(dir, "known-hosts"),
			"-i", filepath.Join(dir, "ssh-key"), "-p", strconv.Itoa(port), p.ss.WindowsUTM.User+"@127.0.0.1",
			"powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ready.Done():
			return ErrReadinessTimeout
		case <-ticker.C:
		}
	}
}
func (p *WindowsUTMProvider) WaitInstance(ctx context.Context, id string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			out, err := p.cmd.Run(ctx, "utmctl", "status", id)
			if err == nil && strings.TrimSpace(string(out)) == "stopped" {
				return nil
			}
		}
	}
}
func (p *WindowsUTMProvider) RemoveInstance(ctx context.Context, id string) error {
	p.mu.Lock()
	dir := p.jitISOs[id]
	p.mu.Unlock()
	if dir == "" {
		return nil
	}

	status, err := p.cmd.Run(ctx, "utmctl", "status", id)
	if err != nil && !strings.HasSuffix(err.Error(), "Virtual machine not found.") {
		return fmt.Errorf("check Windows UTM VM before removal: %w", err)
	}
	if err == nil {
		if strings.TrimSpace(string(status)) != "stopped" {
			// Removal is issued for disposable jobs that are already finished or
			// cancelled. A missing guest shutdown port must not strand the VM.
			if _, err := p.cmd.Run(ctx, "utmctl", "stop", id, "--kill", "--hide"); err != nil {
				return fmt.Errorf("stop Windows UTM VM for removal: %w", err)
			}
		}
		if _, err := p.cmd.Run(ctx, "utmctl", "delete", id, "--hide"); err != nil {
			return fmt.Errorf("delete Windows UTM VM: %w", err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove private Windows job media: %w", err)
	}

	p.mu.Lock()
	delete(p.jitISOs, id)
	delete(p.ports, id)
	p.mu.Unlock()
	return nil
}
func (p *WindowsUTMProvider) Shutdown(ctx context.Context) {
	p.mu.Lock()
	names := make([]string, 0, len(p.ports))
	for n := range p.ports {
		names = append(names, n)
	}
	p.mu.Unlock()
	for _, n := range names {
		if err := p.RemoveInstance(ctx, n); err != nil {
			p.log.Warn("Failed to clean up Windows job VM", "name", n, "error", err)
		}
	}
}

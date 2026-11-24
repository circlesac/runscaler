package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

type utmCommand struct {
	calls       [][]string
	seeds       []map[string]any
	deleteError error
}

func (f *utmCommand) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "utmctl" {
		switch args[0] {
		case "status":
			return []byte("started\n"), nil
		case "delete":
			return nil, f.deleteError
		}
	}
	if name == "ssh-keygen" {
		return nil, os.WriteFile(args[len(args)-1]+".pub", []byte("fixture public key"), 0600)
	}
	if name == "mkisofs" {
		media := args[len(args)-1]
		data, err := os.ReadFile(filepath.Join(media, "windows-seed.json"))
		if err != nil {
			return nil, err
		}
		var seed map[string]any
		if err := json.Unmarshal(data, &seed); err != nil {
			return nil, err
		}
		f.seeds = append(f.seeds, seed)
	}
	return nil, nil
}
func (f *utmCommand) RunStreaming(context.Context, string, ...string) error { return nil }
func TestWindowsUTMStartAndRemoveHaveDynamicLifecycle(t *testing.T) {
	f := &utmCommand{}
	ss := config.ScaleSetConfig{Provider: "windows-utm", WindowsUTM: config.WindowsUTMConfig{Template: "Windows ARM", DocumentsDir: "/utm/documents", PrepareScript: "prepare-utm", User: "runner", PortMin: 2200, PortMax: 2200, RunnerDir: `C:\\actions-runner`, ReadyTimeout: 1}}
	p := NewWindowsUTMProvider(ss, slog.Default())
	p.cmd = f
	id, err := p.StartInstance(context.Background(), "job-1", "jit")
	if err != nil {
		t.Fatal(err)
	}
	if id != "job-1" {
		t.Fatalf("id=%q", id)
	}
	if len(f.calls) < 5 || f.calls[0][0] != "ssh-keygen" || f.calls[1][0] != "mkisofs" || f.calls[2][0] != "utmctl" || f.calls[2][1] != "clone" || f.calls[3][0] != "prepare-utm" || f.calls[4][1] != "start" {
		t.Fatalf("lifecycle=%v", f.calls)
	}
	if got := f.calls[3]; len(got) != 6 || !slices.Equal(got[4:], []string{"--documents-dir", ss.WindowsUTM.DocumentsDir}) {
		t.Fatalf("prepare command=%v", got)
	}
	if err := p.WaitReady(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if check := f.calls[5]; check[0] != "ssh" || !slices.Contains(check, "2200") {
		t.Fatalf("readiness command=%v", check)
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	foundStop, foundDelete := false, false
	for _, c := range f.calls {
		if len(c) > 2 && c[0] == "utmctl" && c[1] == "stop" {
			foundStop = true
		}
		if len(c) > 2 && c[0] == "utmctl" && c[1] == "delete" {
			foundDelete = true
		}
	}
	if !foundStop || !foundDelete {
		t.Fatalf("cleanup=%v", f.calls)
	}
	second, err := p.StartInstance(context.Background(), "job-2", "jit")
	if err != nil {
		t.Fatalf("start second job after releasing its predecessor's port: %v", err)
	}
	if err := p.RemoveInstance(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if len(f.seeds) != 2 || f.seeds[0]["username"] != ss.WindowsUTM.User || len(f.seeds[0]["password"].(string)) < 20 || f.seeds[0]["password"] == f.seeds[1]["password"] {
		t.Fatal("each Windows job must receive a distinct local account password")
	}
}

func TestWindowsUTMRemovalFailureCanBeRetried(t *testing.T) {
	f := &utmCommand{deleteError: errors.New("UTM did not delete the VM")}
	ss := config.ScaleSetConfig{WindowsUTM: config.WindowsUTMConfig{
		Template: "Windows ARM", DocumentsDir: "/utm/documents", PrepareScript: "prepare-utm",
		User: "runner", PortMin: 2200, PortMax: 2200,
	}}
	p := NewWindowsUTMProvider(ss, slog.Default())
	p.cmd = f
	id, err := p.StartInstance(t.Context(), "job-1", "jit")
	if err != nil {
		t.Fatal(err)
	}
	dir := p.jitISOs[id]
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := p.RemoveInstance(t.Context(), id); !errors.Is(err, f.deleteError) {
		t.Fatalf("wanted UTM delete failure, got %v", err)
	}
	if _, err := p.allocatePort("job-2"); err == nil {
		t.Fatal("a VM that failed deletion must retain its port")
	}
	if !slices.ContainsFunc(f.calls, func(call []string) bool {
		return slices.Equal(call, []string{"utmctl", "stop", id, "--kill", "--hide"})
	}) {
		t.Fatal("a remaining disposable VM must be stopped without the guest shutdown port")
	}
	f.deleteError = nil
	if err := p.RemoveInstance(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary SSH and JIT material survived successful removal: %v", err)
	}
	if _, err := p.allocatePort("job-2"); err != nil {
		t.Fatal(err)
	}
}

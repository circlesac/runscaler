//go:build darwin

package provider

import (
	"context"
	"github.com/ysya/runscaler/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSeatbeltLifecycle(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "template")
	state := filepath.Join(root, "jobs")
	os.MkdirAll(filepath.Join(template, "bin"), 0700)
	os.WriteFile(filepath.Join(template, "bin/Runner.Listener"), []byte("fixture"), 0600)
	os.WriteFile(filepath.Join(template, "run.sh"), []byte("#!/bin/sh\nsleep 120 &\necho $! > child.pid\necho 'Listening for Jobs'\nwait\n"), 0700)
	sdk := filepath.Join(root, "sdk")
	os.MkdirAll(sdk, 0700)
	os.WriteFile(filepath.Join(sdk, "source"), []byte("original"), 0600)
	p := NewSeatbeltProvider(config.ScaleSetConfig{Seatbelt: config.SeatbeltConfig{RunnerDir: template, StateDir: state, Path: "/usr/bin:/bin", Environment: map[string]string{"android_home": sdk, "android_sdk_root": sdk}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := p.StartInstance(ctx, "ignored", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(ctx)
	privateSDK := filepath.Join(id, "home", "android-sdk", "source")
	if err := os.WriteFile(privateSDK, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if original, err := os.ReadFile(filepath.Join(sdk, "source")); err != nil || string(original) != "original" {
		t.Fatalf("SDK clone changed source: %q %v", original, err)
	}
	if err = p.WaitReady(ctx, id); err != nil {
		data, _ := os.ReadFile(filepath.Join(id, "runner.log"))
		t.Fatalf("%v: %s exit=%v", err, data, p.instance(id).err)
	}
	if err := os.MkdirAll(filepath.Join(id, "readonly"), 0500); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(id, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	recovery := NewSeatbeltProvider(config.ScaleSetConfig{Seatbelt: p.cfg})
	if err = recovery.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(id); !os.IsNotExist(err) {
		t.Fatal("job directory remains")
	}
	until := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		state, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if !strings.Contains(string(state), "Z") {
			t.Fatalf("child survives cleanup: %s", state)
		}
	}
	if err = p.RemoveInstance(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// System tools that resolve the per-user Darwin temp directory must land in the
// job's private one: /usr/bin/mktemp ignores TMPDIR, and macOS ignores the
// directory suffix in platform binaries.
func TestSeatbeltDarwinTempTools(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "template")
	os.MkdirAll(filepath.Join(template, "bin"), 0700)
	os.WriteFile(filepath.Join(template, "bin/Runner.Listener"), []byte("fixture"), 0600)
	os.WriteFile(filepath.Join(template, "run.sh"), []byte("#!/bin/sh\n"+
		"mktemp -d > mktemp.out 2>&1 || echo FAIL >> mktemp.out\n"+
		"/usr/bin/mktemp -d > system.out 2>&1 || echo FAIL >> system.out\n"+
		"getconf DARWIN_USER_TEMP_DIR > getconf.out\n"+
		"shared=$(/usr/bin/getconf DARWIN_USER_TEMP_DIR)\n"+
		"mkdir \"$shared/TemporaryDirectory.rs$$\" && rmdir \"$shared/TemporaryDirectory.rs$$\" && echo ok > swift.out || echo FAIL > swift.out\n"+
		"mkdir \"$shared/other.rs$$\" 2>/dev/null && rmdir \"$shared/other.rs$$\" && echo WRITABLE > other.out || echo denied > other.out\n"+
		"echo 'Listening for Jobs'\nsleep 120\n"), 0700)
	p := NewSeatbeltProvider(config.ScaleSetConfig{Seatbelt: config.SeatbeltConfig{RunnerDir: template, StateDir: filepath.Join(root, "jobs"), Path: "/usr/bin:/bin"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := p.StartInstance(ctx, "ignored", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(ctx)
	if err = p.WaitReady(ctx, id); err != nil {
		data, _ := os.ReadFile(filepath.Join(id, "runner.log"))
		t.Fatalf("%v: %s", err, data)
	}
	dirs, err := seatbeltUserDirs(id)
	if err != nil {
		t.Fatal(err)
	}
	private := dirs[1]
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(id, name))
		resolved, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
		if err != nil {
			return strings.TrimSpace(string(data))
		}
		return resolved
	}
	made := read("mktemp.out")
	if !strings.HasPrefix(made, private+"/tmp.") {
		t.Fatalf("mktemp -d = %q, want under %s", made, private)
	}
	if got := read("getconf.out"); got != private {
		t.Fatalf("getconf DARWIN_USER_TEMP_DIR = %q, want %s", got, private)
	}
	if got := read("swift.out"); got != "ok" {
		t.Fatalf("Swift driver TemporaryDirectory in the shared temp parent: %q", got)
	}
	if got := read("other.out"); got != "denied" {
		t.Fatalf("other names in the shared temp parent must stay denied: %q", got)
	}
	// The premise: the platform binary itself still resolves the shared parent.
	if got := read("system.out"); !strings.Contains(got, "FAIL") {
		t.Logf("/usr/bin/mktemp -d succeeded (%q); macOS may now honor the suffix in platform binaries", got)
	}
	if err = p.RemoveInstance(ctx, id); err != nil {
		t.Fatal(err)
	}
	// macOS keeps the private root itself (sunlnk); its contents go.
	if _, err = os.Stat(made); !os.IsNotExist(err) {
		t.Fatalf("job temp directory remains: %v", err)
	}
}

// A Developer ID release signs from a job keychain and builds its disk image
// by attaching it inside the job. Removal detaches what a killed job left mounted.
func TestSeatbeltReleaseSigningAndDiskImage(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "template")
	os.MkdirAll(filepath.Join(template, "bin"), 0700)
	os.WriteFile(filepath.Join(template, "bin/Runner.Listener"), []byte("fixture"), 0600)
	os.WriteFile(filepath.Join(template, "run.sh"), []byte(`#!/bin/sh
cd "$TMPDIR"
printf '[req]\ndistinguished_name=dn\nx509_extensions=v3\nprompt=no\n[dn]\nCN=Runscaler Seatbelt Test\n[v3]\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=critical,codeSigning\nbasicConstraints=CA:false\n' > ext.cnf
/usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -keyout k.pem -out c.pem -days 1 -config ext.cnf >/dev/null 2>&1
/usr/bin/openssl pkcs12 -export -inkey k.pem -in c.pem -out id.p12 -passout pass:pw -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1
kc="$TMPDIR/test.keychain-db"
security create-keychain -p "" "$kc" && security unlock-keychain -p "" "$kc"
security import id.p12 -k "$kc" -P pw -T /usr/bin/codesign >/dev/null
security set-key-partition-list -S apple-tool:,apple: -s -k "" "$kc" >/dev/null
cp /usr/bin/true target
codesign --force --keychain "$kc" --sign "Runscaler Seatbelt Test" target > sign.out 2>&1 && echo ok > sign.out
mkdir src mnt left && echo hi > src/a
{ hdiutil create -size 8m -fs HFS+ -volname T -layout NONE -ov rw.dmg &&
  hdiutil attach rw.dmg -nobrowse -noautoopen -mountpoint "$TMPDIR/mnt" &&
  ditto src mnt && hdiutil detach mnt &&
  hdiutil convert rw.dmg -format UDZO -ov -o out.dmg && echo ok; } > dmg.log 2>&1
tail -1 dmg.log > dmg.out
hdiutil create -size 8m -fs HFS+ -volname L -layout NONE -ov left.dmg >/dev/null 2>&1
hdiutil attach left.dmg -nobrowse -noautoopen -mountpoint "$TMPDIR/left" > left.out 2>&1 && echo ok > left.out
echo 'Listening for Jobs'
sleep 120
`), 0700)
	p := NewSeatbeltProvider(config.ScaleSetConfig{Seatbelt: config.SeatbeltConfig{RunnerDir: template, StateDir: filepath.Join(root, "jobs"), Path: "/usr/bin:/bin:/usr/sbin:/sbin"}})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id, err := p.StartInstance(ctx, "ignored", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(ctx)
	if err = p.WaitReady(ctx, id); err != nil {
		data, _ := os.ReadFile(filepath.Join(id, "runner.log"))
		t.Fatalf("%v: %s", err, data)
	}
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(id, "tmp", name))
		return strings.TrimSpace(string(data))
	}
	if got := read("sign.out"); got != "ok" {
		t.Fatalf("codesign from a job keychain: %q", got)
	}
	if got := read("dmg.out"); got != "ok" {
		t.Fatalf("disk image attached inside the job: %q (%s)", got, read("dmg.log"))
	}
	if got := read("left.out"); got != "ok" {
		t.Fatalf("leftover attach: %q", got)
	}
	if err = p.RemoveInstance(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(id); !os.IsNotExist(err) {
		t.Fatalf("job directory remains: %v", err)
	}
}

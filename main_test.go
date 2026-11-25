package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakePlatform struct {
	sleep        bool
	setNoOp      bool
	processes    map[int]processInfo
	started      processInfo
	setCalls     []bool
	stopCalls    []int
	releaseCalls []int
	startCalls   int
	inspectErrs  map[int]error
	setErr       error
	startErr     error
	stopErr      error
}

func (f *fakePlatform) SleepDisabled() (bool, error) { return f.sleep, nil }
func (f *fakePlatform) SetSleepDisabled(value bool) error {
	f.setCalls = append(f.setCalls, value)
	if f.setErr != nil {
		return f.setErr
	}
	if !f.setNoOp {
		f.sleep = value
	}
	return nil
}
func (f *fakePlatform) StartCaffeinate() (processInfo, error) {
	f.startCalls++
	if f.startErr != nil {
		return processInfo{}, f.startErr
	}
	if f.started.PID == 0 {
		return processInfo{}, errors.New("not configured")
	}
	f.processes[f.started.PID] = f.started
	return f.started, nil
}
func (f *fakePlatform) ReleaseProcess(info processInfo) error {
	f.releaseCalls = append(f.releaseCalls, info.PID)
	return nil
}
func (f *fakePlatform) InspectProcess(pid int) (processInfo, error) {
	if err := f.inspectErrs[pid]; err != nil {
		return processInfo{}, err
	}
	info, ok := f.processes[pid]
	if !ok {
		return processInfo{}, errProcessNotFound
	}
	return info, nil
}
func (f *fakePlatform) StopProcess(info processInfo) error {
	f.stopCalls = append(f.stopCalls, info.PID)
	if f.stopErr != nil {
		return f.stopErr
	}
	delete(f.processes, info.PID)
	return nil
}

func newTestApp(t *testing.T, platform *fakePlatform) (application, *bytes.Buffer, *bytes.Buffer, pidStore) {
	t.Helper()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	store := pidStore{path: filepath.Join(t.TempDir(), "keepawake.pid")}
	return application{platform: platform, store: store, stdout: out, stderr: errOut}, out, errOut, store
}

func TestBareCommandDefaultsToReadOnlyStatus(t *testing.T) {
	platform := &fakePlatform{
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"},
	}
	app, out, _, _ := newTestApp(t, platform)
	if code := app.run(nil); code != 0 {
		t.Fatalf("bare keepawake code = %d, want 0; output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "keepawake: OFF") {
		t.Fatalf("bare keepawake output = %q, want OFF", out.String())
	}
	if len(platform.setCalls) != 0 || platform.startCalls != 0 {
		t.Fatalf("bare keepawake mutated state: setCalls=%v startCalls=%d", platform.setCalls, platform.startCalls)
	}
}

func TestStatusReportsOnOffAndPartial(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	tests := []struct {
		name     string
		sleep    bool
		record   *processInfo
		process  *processInfo
		wantCode int
		wantText string
	}{
		{name: "off", wantCode: 0, wantText: "keepawake: OFF"},
		{name: "on", sleep: true, record: &info, process: &info, wantCode: 0, wantText: "keepawake: ON"},
		{name: "missing process", sleep: true, record: &info, wantCode: 1, wantText: "keepawake: PARTIAL"},
		{name: "process only", record: &info, process: &info, wantCode: 1, wantText: "keepawake: PARTIAL"},
		{name: "stale record only", record: &info, wantCode: 1, wantText: "stale pid record"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := &fakePlatform{sleep: test.sleep, processes: map[int]processInfo{}, inspectErrs: map[int]error{}}
			app, out, _, store := newTestApp(t, platform)
			if test.process != nil {
				platform.processes[test.process.PID] = *test.process
			}
			if test.record != nil {
				if err := store.save(*test.record); err != nil {
					t.Fatal(err)
				}
			}
			if code := app.run([]string{"status"}); code != test.wantCode {
				t.Fatalf("status code = %d, want %d; output=%q", code, test.wantCode, out.String())
			}
			if !strings.Contains(out.String(), test.wantText) {
				t.Fatalf("output %q does not contain %q", out.String(), test.wantText)
			}
		})
	}
}

func TestOnPersistsAndReleasesNewManagedProcess(t *testing.T) {
	info := processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     info,
	}
	app, _, _, store := newTestApp(t, platform)
	if code := app.runUnlocked([]string{"on"}); code != 0 {
		t.Fatalf("on code = %d, want 0", code)
	}
	if got := platform.setCalls; len(got) != 1 || !got[0] {
		t.Fatalf("set calls = %v, want [true]", got)
	}
	if len(platform.releaseCalls) != 1 || platform.releaseCalls[0] != 77 {
		t.Fatalf("release calls = %v, want [77]", platform.releaseCalls)
	}
	record, exists, err := store.load()
	if err != nil || !exists || record.PID != 77 || record.Started != "start-77" {
		t.Fatalf("record = %#v, exists=%v, err=%v", record, exists, err)
	}
}

func TestOffStopsVerifiedProcessAndRestoresSleep(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	app, out, _, store := newTestApp(t, platform)
	if err := store.save(info); err != nil {
		t.Fatal(err)
	}
	if code := app.runUnlocked([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, want 0; output=%q", code, out.String())
	}
	if len(platform.stopCalls) != 1 || platform.stopCalls[0] != 42 {
		t.Fatalf("stop calls = %v, want [42]", platform.stopCalls)
	}
	if len(platform.setCalls) != 1 || platform.setCalls[0] {
		t.Fatalf("set calls = %v, want [false]", platform.setCalls)
	}
	if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record remains after verified stop: %v", err)
	}
	out.Reset()
	if code := app.runUnlocked(nil); code != 0 {
		t.Fatalf("status after off code = %d, want 0; output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "keepawake: OFF") {
		t.Fatalf("status after off = %q, want OFF", out.String())
	}
}

func TestOffResetsSleepEvenWhenPreexistingOrLegacySleepBaselineTrue(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	app, out, _, store := newTestApp(t, platform)
	// Legacy v0.1.x record with sleep_disabled_before=true must not trap keepawake off.
	legacyJSON := `{"version":1,"pid":42,"started":"start-42","command":"/usr/bin/caffeinate -i","sleep_disabled_before":true}` + "\n"
	if err := os.WriteFile(store.path, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.run([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, want 0; output=%q", code, out.String())
	}
	if platform.sleep {
		t.Fatal("SleepDisabled remained true after keepawake off")
	}
	if len(platform.stopCalls) != 1 || platform.stopCalls[0] != 42 {
		t.Fatalf("stop calls = %v, want [42]", platform.stopCalls)
	}
	if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record remains after off: %v", err)
	}
	out.Reset()
	if code := app.run([]string{"status"}); code != 0 {
		t.Fatalf("status code = %d, want 0; output=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "keepawake: OFF") {
		t.Fatalf("status output = %q, want OFF", out.String())
	}
}

func TestOffResetsOrphanedSleepDisabledWithNoPIDFile(t *testing.T) {
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
	}
	app, out, _, _ := newTestApp(t, platform)
	if code := app.run([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, want 0; output=%q", code, out.String())
	}
	if platform.sleep {
		t.Fatal("orphaned SleepDisabled was not reset")
	}
	if len(platform.setCalls) != 1 || platform.setCalls[0] {
		t.Fatalf("set calls = %v, want [false]", platform.setCalls)
	}
	out.Reset()
	if code := app.run(nil); code != 0 || !strings.Contains(out.String(), "keepawake: OFF") {
		t.Fatalf("status after off = code %d, output %q", code, out.String())
	}
}

func TestOffNeverSignalsProcessFromStaleRecordButStillResetsSleep(t *testing.T) {
	platform := &fakePlatform{sleep: true, processes: map[int]processInfo{
		// Even another caffeinate process is not ours when its start identity differs.
		42: {PID: 42, Started: "different-start", Command: "/usr/bin/caffeinate -i"},
	}, inspectErrs: map[int]error{}}
	app, out, _, store := newTestApp(t, platform)
	if err := store.save(processInfo{PID: 42, Started: "old-start", Command: "/usr/bin/caffeinate -i"}); err != nil {
		t.Fatal(err)
	}
	if code := app.run([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, want 0; output=%q", code, out.String())
	}
	if len(platform.stopCalls) != 0 {
		t.Fatalf("stale process was signaled: %v", platform.stopCalls)
	}
	if platform.sleep {
		t.Fatal("SleepDisabled was not reset")
	}
	if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale record remains: %v", err)
	}
}

func TestOffIsNoOpWhenAlreadyOff(t *testing.T) {
	platform := &fakePlatform{
		sleep:       false,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
	}
	app, out, _, _ := newTestApp(t, platform)
	if code := app.run([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, want 0; output=%q", code, out.String())
	}
	if len(platform.setCalls) != 0 || len(platform.stopCalls) != 0 {
		t.Fatalf("unexpected mutations when already off: set=%v stop=%v", platform.setCalls, platform.stopCalls)
	}
	if !strings.Contains(out.String(), "already off") {
		t.Fatalf("output = %q, want already off message", out.String())
	}
}

func TestOffFailsIfPmsetDoesNotActuallyRestoreSleep(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		setNoOp:     true,
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	app, _, errOut, store := newTestApp(t, platform)
	if err := store.save(info); err != nil {
		t.Fatal(err)
	}
	if code := app.run([]string{"off"}); code != 1 {
		t.Fatalf("off code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "SleepDisabled is still 1") {
		t.Fatalf("stderr = %q, want verification error", errOut.String())
	}
	if len(platform.stopCalls) != 0 {
		t.Fatalf("process stopped despite failed sleep reset: %v", platform.stopCalls)
	}
	if _, err := os.Stat(store.path); err != nil {
		t.Fatalf("record removed despite failed sleep reset: %v", err)
	}
}

func TestOnFailsIfPmsetDoesNotActuallyDisableSleep(t *testing.T) {
	platform := &fakePlatform{
		sleep:       false,
		setNoOp:     true,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"},
	}
	app, _, errOut, _ := newTestApp(t, platform)
	if code := app.run([]string{"on"}); code != 1 {
		t.Fatalf("on code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "SleepDisabled is still 0") {
		t.Fatalf("stderr = %q, want verification error", errOut.String())
	}
	if platform.startCalls != 0 {
		t.Fatalf("caffeinate started despite failed pmset verification: %d", platform.startCalls)
	}
}

func TestLegacyTempPIDFileIsReadMigratedAndCleaned(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	dir := t.TempDir()
	primary := filepath.Join(dir, "state", "caffeinate.pid")
	legacy := filepath.Join(dir, "tmp", "keepawake.caffeinate.pid")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyStore := pidStore{path: legacy}
	if err := legacyStore.save(info); err != nil {
		t.Fatal(err)
	}

	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	app := application{
		platform: platform,
		store:    pidStore{path: primary, legacyPath: legacy},
		stdout:   out,
		stderr:   errOut,
	}

	// Status finds the legacy live session.
	if code := app.run(nil); code != 0 || !strings.Contains(out.String(), "keepawake: ON") {
		t.Fatalf("status with legacy record = code %d, output %q", code, out.String())
	}

	// On migrates the live record to primary and cleans up legacy.
	out.Reset()
	if code := app.run([]string{"on"}); code != 0 {
		t.Fatalf("on code = %d, output %q", code, out.String())
	}
	if _, err := os.Stat(primary); err != nil {
		t.Fatalf("primary record not created on migrate: %v", err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy record not removed on migrate: %v", err)
	}

	// Off against a fresh legacy-only record stops the process and removes the legacy file.
	if err := os.Remove(primary); err != nil {
		t.Fatal(err)
	}
	if err := legacyStore.save(info); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := app.run([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, output %q", code, out.String())
	}
	if len(platform.stopCalls) != 1 || platform.stopCalls[0] != 42 {
		t.Fatalf("stop calls = %v, want [42]", platform.stopCalls)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy record remains after off: %v", err)
	}
}

func TestRepairCompletesOnWhenEitherComponentIsLive(t *testing.T) {
	newInfo := processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     newInfo,
	}
	app, out, _, store := newTestApp(t, platform)
	if err := os.WriteFile(store.path, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.run([]string{"repair"}); code != 0 {
		t.Fatalf("repair code = %d; output=%q", code, out.String())
	}
	if platform.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", platform.startCalls)
	}
	record, exists, err := store.load()
	if err != nil || !exists || record.PID != 77 || record.Started != "start-77" {
		t.Fatalf("repaired record = %#v, exists=%v, err=%v", record, exists, err)
	}
}

func TestRepairMigratesVerifiedLegacyRecord(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -dimsu"}
	platform := &fakePlatform{
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	app, _, _, store := newTestApp(t, platform)
	if err := os.WriteFile(store.path, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := app.run([]string{"repair"}); code != 0 {
		t.Fatalf("repair code = %d", code)
	}
	if len(platform.setCalls) != 1 || !platform.setCalls[0] {
		t.Fatalf("set calls = %v, want [true]", platform.setCalls)
	}
	record, exists, err := store.load()
	if err != nil || !exists || record.Started != "start-42" {
		t.Fatalf("migrated record = %#v, exists=%v, err=%v", record, exists, err)
	}
}

func TestHelpAndUnknownCommand(t *testing.T) {
	platform := &fakePlatform{processes: map[int]processInfo{}, inspectErrs: map[int]error{}}
	for _, flag := range []string{"help", "-h", "--help"} {
		app, out, errOut, _ := newTestApp(t, platform)
		if code := app.run([]string{flag}); code != 0 {
			t.Fatalf("%s code = %d, want 0", flag, code)
		}
		if !strings.Contains(out.String(), "status|on|off|repair") {
			t.Fatalf("%s stdout = %q, want usage", flag, out.String())
		}
		if errOut.Len() != 0 {
			t.Fatalf("%s unexpected stderr = %q", flag, errOut.String())
		}
	}
	app, _, errOut, _ := newTestApp(t, platform)
	if code := app.run([]string{"wat"}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "status|on|off|repair") {
		t.Fatalf("usage missing commands: %q", errOut.String())
	}
}

func TestParseSleepDisabled(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    bool
		wantErr bool
	}{
		{name: "enabled", output: "SleepDisabled 1\n", want: true},
		{name: "disabled case insensitive", output: "sleepdisabled 0\n", want: false},
		{name: "absent on fresh system", output: "System-wide power settings:\nCurrently in use:\n sleep 1\n", want: false},
		{name: "missing", output: "unexpected output\n", wantErr: true},
		{name: "empty", output: "", wantErr: true},
		{name: "invalid", output: "SleepDisabled maybe\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseSleepDisabled(test.output)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parse error = nil, want error")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parse = %v, err=%v; want %v", got, err, test.want)
			}
		})
	}
}

func TestDarwinSleepDisabledReadsPmsetOutput(t *testing.T) {
	original := combinedOutput
	t.Cleanup(func() { combinedOutput = original })
	var gotName string
	var gotArgs []string
	combinedOutput = func(name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return []byte("SleepDisabled 1\n"), nil
	}

	got, err := (darwinPlatform{}).SleepDisabled()
	if err != nil || !got {
		t.Fatalf("SleepDisabled = %v, err=%v; want true", got, err)
	}
	if gotName != "/usr/bin/pmset" || strings.Join(gotArgs, " ") != "-g" {
		t.Fatalf("pmset command = %s %v, want /usr/bin/pmset [-g]", gotName, gotArgs)
	}
}

func TestDarwinSetSleepDisabledBuildsSudoCommand(t *testing.T) {
	original := runCommand
	t.Cleanup(func() { runCommand = original })
	for _, test := range []struct {
		name     string
		disabled bool
		value    string
	}{
		{name: "enable", disabled: true, value: "1"},
		{name: "disable", disabled: false, value: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got []string
			runCommand = func(cmd *exec.Cmd) error {
				got = append([]string(nil), cmd.Args...)
				return nil
			}
			if err := (darwinPlatform{}).SetSleepDisabled(test.disabled); err != nil {
				t.Fatal(err)
			}
			want := []string{"/usr/bin/sudo", "/usr/bin/pmset", "-a", "disablesleep", test.value}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("command = %v, want %v", got, want)
			}
		})
	}
}

func TestNativeProcessHelpersFailSafeForInvalidOrUnownedProcess(t *testing.T) {
	if _, err := (darwinPlatform{}).InspectProcess(1); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("InspectProcess invalid pid error = %v, want errProcessNotFound", err)
	}
	if err := (darwinPlatform{}).StopProcess(processInfo{PID: 1}); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("StopProcess invalid process error = %v, want errProcessNotFound", err)
	}
	if err := (darwinPlatform{}).ReleaseProcess(processInfo{}); err != nil {
		t.Fatalf("ReleaseProcess without owned handle = %v, want nil", err)
	}
}

func TestDefaultPIDPathHonorsOverrideAndPersistentStateDir(t *testing.T) {
	t.Setenv("KEEPAWAKE_PIDFILE", "/tmp/keepawake-review.pid")
	if got := defaultPIDPath(); got != "/tmp/keepawake-review.pid" {
		t.Fatalf("override pid path = %q", got)
	}
	if got := defaultPIDStore().legacyPath; got != "" {
		t.Fatalf("override legacy path = %q, want empty", got)
	}

	t.Setenv("KEEPAWAKE_PIDFILE", "")
	xdgDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdgDir)
	wantXDG := filepath.Join(xdgDir, "keepawake", "caffeinate.pid")
	if got := defaultPIDPath(); got != wantXDG {
		t.Fatalf("XDG pid path = %q, want %q", got, wantXDG)
	}

	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantHome := filepath.Join(home, ".local", "state", "keepawake", "caffeinate.pid")
	if got := defaultPIDPath(); got != wantHome {
		t.Fatalf("home state pid path = %q, want %q", got, wantHome)
	}
	wantLegacy := filepath.Join(os.TempDir(), "keepawake.caffeinate.pid")
	if got := defaultPIDStore().legacyPath; got != wantLegacy {
		t.Fatalf("legacy pid path = %q, want %q", got, wantLegacy)
	}
}

func TestOnRollsBackSleepWhenCaffeinateStartFails(t *testing.T) {
	platform := &fakePlatform{
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		startErr:    errors.New("start failed"),
	}
	app, _, _, _ := newTestApp(t, platform)
	if code := app.runUnlocked([]string{"on"}); code != 1 {
		t.Fatalf("on code = %d, want 1", code)
	}
	if got := platform.setCalls; len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("set calls = %v, want [true false]", got)
	}
}

func TestOnRollsBackSleepWhenStaleRecordCannotBeRemoved(t *testing.T) {
	platform := &fakePlatform{processes: map[int]processInfo{}, inspectErrs: map[int]error{}}
	app, _, _, store := newTestApp(t, platform)
	if err := os.WriteFile(store.path, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(store.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if code := app.runUnlocked([]string{"on"}); code != 1 {
		t.Fatalf("on code = %d, want 1", code)
	}
	if got := platform.setCalls; len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("set calls = %v, want [true false]", got)
	}
}

func TestRepairStopsNewProcessWhenRecordSaveFails(t *testing.T) {
	info := processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     info,
	}
	app, _, _, _ := newTestApp(t, platform)
	app.store.path = t.TempDir() // Renaming a PID record over a directory must fail.
	if code := app.runUnlocked([]string{"repair"}); code != 1 {
		t.Fatalf("repair code = %d, want 1", code)
	}
	if len(platform.stopCalls) != 1 || platform.stopCalls[0] != 77 {
		t.Fatalf("stop calls = %v, want [77]", platform.stopCalls)
	}
}

func TestRepairRollsBackNewSleepSettingWhenRecordSaveFails(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	app, _, _, store := newTestApp(t, platform)
	if err := store.save(info); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(store.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if code := app.runUnlocked([]string{"repair"}); code != 1 {
		t.Fatalf("repair code = %d, want 1", code)
	}
	if got := platform.setCalls; len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("set calls = %v, want [true false]", got)
	}
}

func TestConcurrentInvocationFailsClosed(t *testing.T) {
	platform := &fakePlatform{processes: map[int]processInfo{}, inspectErrs: map[int]error{}}
	app, _, errOut, store := newTestApp(t, platform)
	unlock, err := store.tryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if code := app.run([]string{"status"}); code != 1 {
		t.Fatalf("status code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "already running") {
		t.Fatalf("missing lock error: %q", errOut.String())
	}
}

func TestDarwinCaffeinateLifecycleIsVerified(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keepawake is a macOS-only command")
	}
	platform := darwinPlatform{}
	info, err := platform.StartCaffeinate()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process, findErr := os.FindProcess(info.PID); findErr == nil {
			_ = process.Kill()
		}
	})
	if !isCaffeinate(info.Command) || info.Started == "" {
		t.Fatalf("unverified process identity: %#v", info)
	}
	if err := platform.StopProcess(info); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.InspectProcess(info.PID); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("caffeinate still appears live: %v", err)
	}
}

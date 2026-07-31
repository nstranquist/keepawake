package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakePlatform struct {
	sleep        bool
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
	f.sleep = value
	f.setCalls = append(f.setCalls, value)
	return f.setErr
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
	if record.SleepDisabledBefore == nil || *record.SleepDisabledBefore {
		t.Fatalf("sleep baseline = %#v, want false", record.SleepDisabledBefore)
	}
}

func TestOffStopsVerifiedProcessBeforeRemovingRecord(t *testing.T) {
	info := processInfo{PID: 42, Started: "start-42", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{42: info},
		inspectErrs: map[int]error{},
	}
	app, _, _, store := newTestApp(t, platform)
	if err := store.saveManaged(info, false); err != nil {
		t.Fatal(err)
	}
	if code := app.runUnlocked([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d, want 0", code)
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
}

func TestOffNeverSignalsProcessFromStaleRecord(t *testing.T) {
	platform := &fakePlatform{sleep: true, processes: map[int]processInfo{
		// Even another caffeinate process is not ours when its start identity differs.
		42: {PID: 42, Started: "different-start", Command: "/usr/bin/caffeinate -i"},
	}, inspectErrs: map[int]error{}}
	app, out, _, store := newTestApp(t, platform)
	if err := store.save(processInfo{PID: 42, Started: "old-start", Command: "/usr/bin/caffeinate -i"}); err != nil {
		t.Fatal(err)
	}
	if code := app.run([]string{"off"}); code != 1 {
		t.Fatalf("off code = %d, want 1; output=%q", code, out.String())
	}
	if len(platform.stopCalls) != 0 {
		t.Fatalf("stale process was signaled: %v", platform.stopCalls)
	}
	if !platform.sleep {
		t.Fatal("unknown sleep ownership was changed")
	}
	if !strings.Contains(out.String(), "previous ownership is unknown") {
		t.Fatalf("missing unknown ownership warning: %q", out.String())
	}
	if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale record remains: %v", err)
	}
}

func TestOnAndOffPreservePreexistingSleepSetting(t *testing.T) {
	info := processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     info,
	}
	app, out, _, store := newTestApp(t, platform)
	if code := app.runUnlocked([]string{"on"}); code != 0 {
		t.Fatalf("on code = %d; output=%q", code, out.String())
	}
	record, exists, err := store.load()
	if err != nil || !exists || record.SleepDisabledBefore == nil || !*record.SleepDisabledBefore {
		t.Fatalf("record = %#v, exists=%v, err=%v; want true baseline", record, exists, err)
	}
	if code := app.runUnlocked([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d; output=%q", code, out.String())
	}
	if len(platform.setCalls) != 0 {
		t.Fatalf("set calls = %v, want no power-setting mutation", platform.setCalls)
	}
	if !platform.sleep {
		t.Fatal("pre-existing sleep setting was changed")
	}
	if !strings.Contains(out.String(), "pre-existing lid-close sleep disable preserved") {
		t.Fatalf("missing preservation output: %q", out.String())
	}
}

func TestOnPreservesKnownSleepBaselineAfterStaleProcess(t *testing.T) {
	info := processInfo{PID: 77, Started: "start-77", Command: "/usr/bin/caffeinate -i"}
	platform := &fakePlatform{
		sleep:       true,
		processes:   map[int]processInfo{},
		inspectErrs: map[int]error{},
		started:     info,
	}
	app, _, _, store := newTestApp(t, platform)
	if err := store.saveManaged(info, false); err != nil {
		t.Fatal(err)
	}
	if code := app.runUnlocked([]string{"on"}); code != 0 {
		t.Fatalf("on code = %d", code)
	}
	if code := app.runUnlocked([]string{"off"}); code != 0 {
		t.Fatalf("off code = %d", code)
	}
	if len(platform.setCalls) != 1 || platform.setCalls[0] {
		t.Fatalf("set calls = %v, want [false]", platform.setCalls)
	}
	if platform.sleep {
		t.Fatal("known sleep baseline was not restored")
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
	if record.SleepDisabledBefore == nil || *record.SleepDisabledBefore {
		t.Fatalf("migrated sleep baseline = %#v, want false", record.SleepDisabledBefore)
	}
}

func TestUnknownCommandReturnsUsage(t *testing.T) {
	platform := &fakePlatform{processes: map[int]processInfo{}, inspectErrs: map[int]error{}}
	app, _, errOut, _ := newTestApp(t, platform)
	if code := app.run([]string{"wat"}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "on|off|status|repair") {
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
		{name: "missing", output: "System-wide power settings:\n", wantErr: true},
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

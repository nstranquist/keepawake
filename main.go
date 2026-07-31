package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const pidRecordVersion = 1

var errProcessNotFound = errors.New("process not found")

type pidRecord struct {
	Version int    `json:"version"`
	PID     int    `json:"pid"`
	Started string `json:"started,omitempty"`
	Command string `json:"command,omitempty"`
}

type processInfo struct {
	PID     int
	Started string
	Command string
	process *os.Process
}

type powerPlatform interface {
	SleepDisabled() (bool, error)
	SetSleepDisabled(bool) error
	StartCaffeinate() (processInfo, error)
	ReleaseProcess(processInfo) error
	InspectProcess(int) (processInfo, error)
	StopProcess(processInfo) error
}

type pidStore struct {
	path string
}

func (s pidStore) load() (pidRecord, bool, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return pidRecord{}, false, nil
	}
	if err != nil {
		return pidRecord{}, false, err
	}

	trimmed := strings.TrimSpace(string(data))
	if pid, err := strconv.Atoi(trimmed); err == nil {
		if pid <= 1 {
			return pidRecord{}, true, fmt.Errorf("unsafe legacy pid %d", pid)
		}
		return pidRecord{PID: pid}, true, nil
	}

	var record pidRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return pidRecord{}, true, fmt.Errorf("invalid pid record: %w", err)
	}
	if record.Version != pidRecordVersion || record.PID <= 1 || record.Started == "" {
		return pidRecord{}, true, errors.New("invalid pid record fields")
	}
	return record, true, nil
}

func (s pidStore) save(info processInfo) error {
	record := pidRecord{
		Version: pidRecordVersion,
		PID:     info.PID,
		Started: info.Started,
		Command: info.Command,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".keepawake-pid-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s pidStore) remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s pidStore) tryLock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another keepawake command is already running")
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

type state struct {
	sleepDisabled bool
	pidFile       bool
	process       *processInfo
	stale         bool
}

type application struct {
	platform powerPlatform
	store    pidStore
	stdout   io.Writer
	stderr   io.Writer
}

func (a application) inspect() (state, error) {
	disabled, err := a.platform.SleepDisabled()
	if err != nil {
		return state{}, err
	}
	record, exists, loadErr := a.store.load()
	result := state{sleepDisabled: disabled, pidFile: exists}
	if loadErr != nil {
		result.stale = exists
		return result, nil
	}
	if !exists {
		return result, nil
	}
	info, err := a.platform.InspectProcess(record.PID)
	if err != nil || !isCaffeinate(info.Command) {
		result.stale = true
		return result, nil
	}
	if record.Started != "" && record.Started != info.Started {
		result.stale = true
		return result, nil
	}
	result.process = &info
	return result, nil
}

func isCaffeinate(command string) bool {
	fields := strings.Fields(command)
	return len(fields) > 0 && filepath.Base(fields[0]) == "caffeinate"
}

func (a application) run(args []string) int {
	unlock, err := a.store.tryLock()
	if err != nil {
		return a.fail(err)
	}
	defer unlock()
	return a.runUnlocked(args)
}

func (a application) runUnlocked(args []string) int {
	action := "on"
	if len(args) > 0 {
		action = args[0]
	}
	if len(args) > 1 {
		return a.usage()
	}

	switch action {
	case "on", "start":
		return a.on()
	case "off", "stop":
		return a.off()
	case "status":
		return a.status()
	case "repair":
		return a.repair()
	default:
		return a.usage()
	}
}

func (a application) usage() int {
	fmt.Fprintln(a.stderr, "keepawake: usage: keepawake [on|off|status|repair]")
	return 2
}

func (a application) on() int {
	current, err := a.inspect()
	if err != nil {
		return a.fail(err)
	}
	enabledByCommand := !current.sleepDisabled
	if enabledByCommand {
		if err := a.platform.SetSleepDisabled(true); err != nil {
			return a.fail(fmt.Errorf("failed to disable lid-close sleep: %w", err))
		}
	}
	if current.process != nil {
		if err := a.store.save(*current.process); err != nil {
			return a.fail(a.rollbackError(fmt.Errorf("failed to persist process identity: %w", err), enabledByCommand, nil))
		}
		fmt.Fprintf(a.stdout, "keepawake: ON — already running (caffeinate pid %d)\n", current.process.PID)
		fmt.Fprintln(a.stdout, "keepawake: lid can be closed. Run 'keepawake off' when finished (no airflow while shut).")
		return 0
	}
	if current.pidFile {
		if err := a.store.remove(); err != nil {
			return a.fail(a.rollbackError(fmt.Errorf("failed to remove stale pid record: %w", err), enabledByCommand, nil))
		}
	}
	info, err := a.platform.StartCaffeinate()
	if err != nil {
		return a.fail(a.rollbackError(fmt.Errorf("failed to start caffeinate: %w", err), enabledByCommand, nil))
	}
	if err := a.store.save(info); err != nil {
		return a.fail(a.rollbackError(fmt.Errorf("failed to persist process identity: %w", err), enabledByCommand, &info))
	}
	if err := a.platform.ReleaseProcess(info); err != nil {
		_ = a.store.remove()
		return a.fail(a.rollbackError(fmt.Errorf("failed to detach caffeinate process: %w", err), enabledByCommand, &info))
	}
	fmt.Fprintf(a.stdout, "keepawake: ON — lid-close sleep disabled, caffeinate pid %d\n", info.PID)
	fmt.Fprintln(a.stdout, "keepawake: lid can be closed. Run 'keepawake off' when finished (no airflow while shut).")
	return 0
}

func (a application) off() int {
	current, err := a.inspect()
	if err != nil {
		return a.fail(err)
	}
	if current.sleepDisabled {
		if err := a.platform.SetSleepDisabled(false); err != nil {
			return a.fail(fmt.Errorf("failed to restore lid-close sleep: %w", err))
		}
	}
	if current.process != nil {
		if err := a.platform.StopProcess(*current.process); err != nil && !errors.Is(err, errProcessNotFound) {
			return a.fail(fmt.Errorf("failed to stop managed caffeinate process: %w", err))
		}
	}
	if current.pidFile {
		if err := a.store.remove(); err != nil {
			return a.fail(fmt.Errorf("failed to remove pid record: %w", err))
		}
	}
	fmt.Fprintln(a.stdout, "keepawake: OFF — normal lid-close sleep restored.")
	return 0
}

func (a application) status() int {
	current, err := a.inspect()
	if err != nil {
		return a.fail(err)
	}
	if current.sleepDisabled && current.process != nil && !current.stale {
		fmt.Fprintf(a.stdout, "keepawake: ON — lid-close sleep disabled, caffeinate pid %d\n", current.process.PID)
		return 0
	}
	if !current.sleepDisabled && current.process == nil && !current.stale {
		fmt.Fprintln(a.stdout, "keepawake: OFF — normal lid-close sleep enabled, caffeinate not running")
		return 0
	}
	process := "not running"
	if current.process != nil {
		process = fmt.Sprintf("pid %d", current.process.PID)
	} else if current.stale {
		process = "stale pid record"
	}
	fmt.Fprintf(a.stdout, "keepawake: PARTIAL — settings disagree (SleepDisabled=%d, caffeinate=%s); run 'keepawake repair'\n", boolInt(current.sleepDisabled), process)
	return 1
}

func (a application) repair() int {
	current, err := a.inspect()
	if err != nil {
		return a.fail(err)
	}
	// Any live component means the last intended state was most likely ON.
	// Otherwise repair only removes stale metadata and preserves OFF.
	if current.sleepDisabled || current.process != nil {
		enabledByCommand := !current.sleepDisabled
		if enabledByCommand {
			if err := a.platform.SetSleepDisabled(true); err != nil {
				return a.fail(fmt.Errorf("failed to disable lid-close sleep: %w", err))
			}
		}
		info := current.process
		startedByCommand := false
		if info == nil {
			started, err := a.platform.StartCaffeinate()
			if err != nil {
				return a.fail(a.rollbackError(fmt.Errorf("failed to start caffeinate: %w", err), enabledByCommand, nil))
			}
			info = &started
			startedByCommand = true
		}
		if err := a.store.save(*info); err != nil {
			var started *processInfo
			if startedByCommand {
				started = info
			}
			return a.fail(a.rollbackError(fmt.Errorf("failed to persist process identity: %w", err), enabledByCommand, started))
		}
		if startedByCommand {
			if err := a.platform.ReleaseProcess(*info); err != nil {
				_ = a.store.remove()
				return a.fail(a.rollbackError(fmt.Errorf("failed to detach caffeinate process: %w", err), enabledByCommand, info))
			}
		}
		fmt.Fprintf(a.stdout, "keepawake: ON — repaired and verified (caffeinate pid %d)\n", info.PID)
		return 0
	}
	if current.pidFile {
		if err := a.store.remove(); err != nil {
			return a.fail(fmt.Errorf("failed to remove stale pid record: %w", err))
		}
	}
	fmt.Fprintln(a.stdout, "keepawake: OFF — stale metadata removed; normal lid-close sleep is enabled")
	return 0
}

func (a application) rollbackError(primary error, disableSleep bool, started *processInfo) error {
	var cleanup []string
	if started != nil {
		if err := a.platform.StopProcess(*started); err != nil && !errors.Is(err, errProcessNotFound) {
			cleanup = append(cleanup, "stop caffeinate: "+err.Error())
		}
	}
	if disableSleep {
		if err := a.platform.SetSleepDisabled(false); err != nil {
			cleanup = append(cleanup, "restore lid-close sleep: "+err.Error())
		}
	}
	if len(cleanup) == 0 {
		return primary
	}
	return fmt.Errorf("%w; rollback failed (%s)", primary, strings.Join(cleanup, "; "))
}

func (a application) fail(err error) int {
	fmt.Fprintf(a.stderr, "keepawake: %v\n", err)
	return 1
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type darwinPlatform struct{}

func (darwinPlatform) SleepDisabled() (bool, error) {
	output, err := exec.Command("/usr/bin/pmset", "-g").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("pmset -g: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "SleepDisabled") {
			return fields[1] == "1", nil
		}
	}
	return false, nil
}

func (darwinPlatform) SetSleepDisabled(disabled bool) error {
	value := "0"
	if disabled {
		value = "1"
	}
	cmd := exec.Command("/usr/bin/sudo", "/usr/bin/pmset", "-a", "disablesleep", value)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (p darwinPlatform) StartCaffeinate() (processInfo, error) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return processInfo{}, err
	}
	defer devNull.Close()
	cmd := exec.Command("/usr/bin/caffeinate", "-i")
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		return processInfo{}, err
	}
	pid := cmd.Process.Pid
	for attempt := 0; attempt < 10; attempt++ {
		info, err := p.InspectProcess(pid)
		if err == nil {
			info.process = cmd.Process
			return info, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return processInfo{}, errors.New("caffeinate started but its process identity could not be verified")
}

func (darwinPlatform) ReleaseProcess(info processInfo) error {
	if info.process == nil {
		return nil
	}
	return info.process.Release()
}

func (darwinPlatform) InspectProcess(pid int) (processInfo, error) {
	if pid <= 1 {
		return processInfo{}, errProcessNotFound
	}
	started, err := psField(pid, "lstart")
	if err != nil {
		return processInfo{}, err
	}
	command, err := psField(pid, "command")
	if err != nil {
		return processInfo{}, err
	}
	return processInfo{PID: pid, Started: started, Command: command}, nil
}

func psField(pid int, field string) (string, error) {
	output, err := exec.Command("/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", field+"=").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return "", errProcessNotFound
	}
	return strings.TrimSpace(string(output)), nil
}

func (p darwinPlatform) StopProcess(expected processInfo) error {
	if expected.PID <= 1 || expected.Started == "" {
		return errProcessNotFound
	}
	current, err := p.InspectProcess(expected.PID)
	if errors.Is(err, errProcessNotFound) {
		return errProcessNotFound
	}
	if err != nil {
		return err
	}
	if current.Started != expected.Started || !isCaffeinate(current.Command) {
		return errors.New("managed process identity changed; refusing to signal it")
	}
	process, err := os.FindProcess(expected.PID)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return errProcessNotFound
		}
		return err
	}
	if expected.process != nil {
		waited := make(chan error, 1)
		go func() {
			_, waitErr := expected.process.Wait()
			waited <- waitErr
		}()
		select {
		case err := <-waited:
			return err
		case <-time.After(time.Second):
			_ = expected.process.Kill()
			select {
			case <-waited:
				return nil
			case <-time.After(time.Second):
				return errors.New("managed caffeinate process could not be reaped after SIGKILL")
			}
		}
	}
	for attempt := 0; attempt < 50; attempt++ {
		time.Sleep(20 * time.Millisecond)
		current, err := p.InspectProcess(expected.PID)
		if errors.Is(err, errProcessNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Started != expected.Started {
			return nil
		}
	}
	return errors.New("managed caffeinate process did not exit after SIGTERM")
}

func defaultPIDPath() string {
	if override := os.Getenv("KEEPAWAKE_PIDFILE"); override != "" {
		return override
	}
	return filepath.Join(os.TempDir(), "keepawake.caffeinate.pid")
}

func main() {
	app := application{
		platform: darwinPlatform{},
		store:    pidStore{path: defaultPIDPath()},
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}
	os.Exit(app.run(os.Args[1:]))
}

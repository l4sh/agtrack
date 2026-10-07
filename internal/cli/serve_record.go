package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/l4sh/agtrack/internal/db"
)

// A running `agtrack serve` records itself next to the database
// (<db>.serve.json, mode 0600) so a later `agtrack serve` or `spec serve`
// reuses it instead of starting another server. The record is trusted only
// after the process is alive and answers GET /api/ping with its token; a
// record that fails either check is stale and is deleted.

type serveRecord struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Token     string `json:"token"`
	As        string `json:"as"`
	Build     string `json:"build"`
	DB        string `json:"db"`
	StartedAt string `json:"started_at"`
}

func (r *serveRecord) base() string { return fmt.Sprintf("http://127.0.0.1:%d", r.Port) }

func serveRecordPath(dbPath string) string { return db.ResolvePath(dbPath) + ".serve.json" }

// buildID names the running binary well enough to notice a rebuild: the
// version plus the executable's size and modification time.
func buildID() string {
	exe, err := os.Executable()
	if err != nil {
		return Version
	}
	st, err := os.Stat(exe)
	if err != nil {
		return Version
	}
	return fmt.Sprintf("%s+%d-%d", Version, st.Size(), st.ModTime().Unix())
}

// liveServeRecord returns the recorded server if it is really running, and
// otherwise deletes the stale record and returns nil.
func liveServeRecord(path string) *serveRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r serveRecord
	if json.Unmarshal(data, &r) != nil || r.PID <= 0 || r.Port <= 0 || r.Token == "" || !pingServer(&r) {
		os.Remove(path)
		return nil
	}
	return &r
}

// pingServer: the process exists and the server on the recorded port is
// that process, holding the recorded token.
func pingServer(r *serveRecord) bool {
	if err := syscall.Kill(r.PID, 0); err != nil && err != syscall.EPERM {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, r.base()+"/api/ping", nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Agtrack-Token", r.Token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var got struct {
		PID int `json:"pid"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&got) == nil && got.PID == r.PID
}

// writeServeRecord writes the record atomically, readable by the owner only.
func writeServeRecord(path string, r *serveRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".serve-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// removeServeRecord deletes the record only if it is still this process's.
func removeServeRecord(path string, pid int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var r serveRecord
	if json.Unmarshal(data, &r) == nil && r.PID == pid {
		os.Remove(path)
	}
}

// stopServer asks a recorded server to exit and waits until it is gone.
func stopServer(r *serveRecord) error {
	if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	for i := 0; i < 50; i++ {
		if !pingServer(r) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("agtrack serve (pid %d) did not stop", r.PID)
}

var (
	exitMu    sync.Mutex
	exitHooks []func()
)

// AtExit registers cleanup for an interrupted run: main exits on SIGINT and
// SIGTERM without unwinding the stack, so deferred calls do not run.
func AtExit(f func()) {
	exitMu.Lock()
	exitHooks = append(exitHooks, f)
	exitMu.Unlock()
}

// RunExitHooks runs the registered cleanup once; main calls it on a signal.
func RunExitHooks() {
	exitMu.Lock()
	hooks := exitHooks
	exitHooks = nil
	exitMu.Unlock()
	for _, f := range hooks {
		f()
	}
}

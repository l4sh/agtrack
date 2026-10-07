package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func writeRecord(t *testing.T, path string, r serveRecord) {
	t.Helper()
	if err := writeServeRecord(path, &r); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("record mode %v, want 0600", st.Mode().Perm())
	}
}

// pingStub answers /api/ping like a live server with the given pid and token.
func pingStub(t *testing.T, pid int, token string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" || r.Header.Get("X-Agtrack-Token") != token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"pid": pid})
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

func TestServeRecordLiveIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db.serve.json")
	port := pingStub(t, os.Getpid(), "tok")
	writeRecord(t, path, serveRecord{PID: os.Getpid(), Port: port, Token: "tok", As: "human"})
	if r := liveServeRecord(path); r == nil || r.Port != port {
		t.Fatalf("live record not returned: %+v", r)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("live record removed: %v", err)
	}
}

func TestServeRecordStaleIsRemoved(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip("no `true` binary")
	}
	livePort := pingStub(t, os.Getpid(), "tok")
	cases := map[string]serveRecord{
		// The process is gone.
		"dead pid": {PID: dead.Process.Pid, Port: livePort, Token: "tok"},
		// The process is alive but nothing listens on the port.
		"no server": {PID: os.Getpid(), Port: 1, Token: "tok"},
		// Something listens, but it is another process's server.
		"other pid": {PID: os.Getppid(), Port: livePort, Token: "tok"},
		// The server does not accept the recorded token.
		"wrong token": {PID: os.Getpid(), Port: livePort, Token: "nope"},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.db.serve.json")
			writeRecord(t, path, rec)
			if r := liveServeRecord(path); r != nil {
				t.Fatalf("stale record returned: %+v", r)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("stale record not removed (%v)", err)
			}
		})
	}
	t.Run("garbage", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "t.db.serve.json")
		os.WriteFile(path, []byte("{not json"), 0o600)
		if liveServeRecord(path) != nil {
			t.Fatal("garbage accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("garbage record not removed")
		}
	})
}

func TestServeRecordRemoveOnlyOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db.serve.json")
	writeRecord(t, path, serveRecord{PID: 4242, Port: 1, Token: "x"})
	removeServeRecord(path, 4243)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("another process's record was removed")
	}
	removeServeRecord(path, 4242)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("own record kept: %v", err)
	}
}

package startup

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolvePortAvoidsLegacyAndOtherWorkspaces(t *testing.T) {
	current := Identity(t.TempDir())
	other := Identity(t.TempDir())
	older := current
	older.Build = "old-build"
	start := func(reply any) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/health" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(reply)
		}))
		t.Cleanup(server.Close)
		_, port, err := net.SplitHostPort(server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return port
	}
	preferred := start(map[string]string{"status": "ok", "service": current.Service})
	// 旧版、其他构建及其他解压目录均不能复用。
	for _, port := range []string{preferred, start(older), start(other)} {
		if reuse, listen := ResolvePort(port, "", current); reuse != "" || listen != "0" {
			t.Fatalf("预期系统分配空闲端口，实际 reuse=%q listen=%q", reuse, listen)
		}
	}
	// 新服务已占用备用端口时，双击应复用正确实例。
	previous := start(current)
	if reuse, listen := ResolvePort(preferred, previous, current); reuse != previous || listen != "" {
		t.Fatalf("未复用当前服务：reuse=%q listen=%q", reuse, listen)
	}
}

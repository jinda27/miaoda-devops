package startup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// BuildID 在 Windows 打包时同时写入启动器和服务。
var BuildID = "dev"

type Health struct {
	Status    string `json:"status"`
	Service   string `json:"service"`
	Build     string `json:"build"`
	Workspace string `json:"workspace"`
}

func Identity(root string) Health {
	root = filepath.Clean(root)
	// Windows 路径大小写不影响同一工作区的识别。
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	hash := sha256.Sum256([]byte(root))
	return Health{"ok", "miaoda-deployment-platform", BuildID, hex.EncodeToString(hash[:])}
}

func ValidPort(port string) bool {
	number, err := strconv.Atoi(port)
	return err == nil && number >= 1 && number <= 65535 && strconv.Itoa(number) == port
}

func URL(port string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/"
}

func ResolvePort(preferred, previous string, expected Health) (reuse, listen string) {
	// 只有健康检查和身份同时匹配才复用，旧版或其他程序保持运行。
	if previous != preferred && Reusable(previous, expected) {
		return previous, ""
	}
	if Reusable(preferred, expected) {
		return preferred, ""
	}
	// 端口已占用时，不预先抢占备用端口，交给新服务绑定。
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", preferred), time.Second)
	if err == nil {
		connection.Close()
		return "", "0"
	}
	return "", preferred
}

func Reusable(port string, expected Health) bool {
	if !ValidPort(port) {
		return false
	}
	client := http.Client{
		Timeout:       2 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Get(URL(port) + "api/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	// 仅复用同一工作区、同一构建的服务；旧版没有这些字段。
	var actual Health
	return response.StatusCode == http.StatusOK &&
		json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&actual) == nil && actual == expected
}

package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"app-deployment-platform/internal/deployment/application"
	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
	httpapi "app-deployment-platform/internal/deployment/interfaces/http"
	environment "app-deployment-platform/internal/environment/application"
	"app-deployment-platform/internal/environment/infrastructure"
	"app-deployment-platform/internal/startup"
)

func main() {
	larkcli.ClearInheritedCredentials()
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	projectDir, err := projectDirectory()
	if err != nil {
		log.Fatal(err)
	}
	logDir := filepath.Join(projectDir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		log.Fatalf("创建日志目录失败：%v", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Fatalf("打开服务日志失败：%v", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(logFile, os.Stderr))

	port := os.Getenv("PORT")
	if port == "" {
		port = "18080"
	}
	address := net.JoinHostPort("127.0.0.1", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()

	tools, err := infrastructure.Configure(projectDir, os.Getenv("MIAODA_PACKAGES_DIR"))
	if err != nil {
		log.Fatal(err)
	}
	service, err := application.NewService(filepath.Join(projectDir, "data"))
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.RegisterRoutes(mux, service, environment.NewService(tools), startup.Identity(projectDir))
	webDir := os.Getenv("MIAODA_WEB_DIR")
	if webDir == "" {
		webDir = filepath.Join(projectDir, "fe", "dist")
	}
	mux.Handle("GET /", http.FileServer(http.Dir(webDir)))

	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           httpapi.LocalOnly(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("[service] started address=http://%s", server.Addr)
	mode := os.Getenv("MIAODA_DESKTOP_STARTUP")
	// Windows 支持动态端口，Mac 启动器继续接收原有的就绪信号。
	if mode == "1" || mode == "2" {
		signal := "ready"
		if mode == "2" {
			signal = fmt.Sprintf("ready %d", listener.Addr().(*net.TCPAddr).Port)
		}
		if _, err := fmt.Fprintln(os.Stdout, signal); err != nil {
			log.Fatalf("无法通知桌面启动器：%v", err)
		}
		log.SetOutput(logFile)
	}
	log.Fatal(server.Serve(listener))
}

func projectDirectory() (string, error) {
	// 桌面程序显式指定数据目录，源码启动继续沿用项目目录。
	if root := os.Getenv("MIAODA_PROJECT_DIR"); root != "" {
		return filepath.Abs(root)
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if filepath.Base(current) == "be" {
		return filepath.Abs("..")
	}
	return current, nil
}

package httpapi

import (
	"net/http"

	"app-deployment-platform/internal/deployment/application"
	environment "app-deployment-platform/internal/environment/application"
	"app-deployment-platform/internal/startup"
)

func RegisterRoutes(mux *http.ServeMux, service *application.Service, environmentService *environment.Service, identity startup.Health) {
	registerEnvironment(mux, environmentService)
	handler := NewHandler(service, identity)
	mux.HandleFunc("GET /api/health", handler.health)
	mux.HandleFunc("GET /api/cli", handler.cliStatus)
	mux.HandleFunc("POST /api/tenants", handler.createTenant)
	mux.HandleFunc("POST /api/tenants/initialize", handler.initializeTenant)
	mux.HandleFunc("GET /api/tenants", handler.currentTenant)
	mux.HandleFunc("GET /api/tenants/{id}", handler.tenantStatus)
	mux.HandleFunc("POST /api/tenants/{id}/refresh", handler.refreshTenant)
	mux.HandleFunc("POST /api/tenants/{id}/cancel", handler.cancelTenant)
	mux.HandleFunc("POST /api/auth", handler.login)
	mux.HandleFunc("GET /api/auth/{id}", handler.loginStatus)
	mux.HandleFunc("POST /api/auth/{id}/refresh", handler.refreshLogin)
	mux.HandleFunc("GET /api/apps", handler.apps)
	mux.HandleFunc("GET /api/jobs", handler.jobs)
	mux.HandleFunc("POST /api/package", handler.startPackage)
	mux.HandleFunc("POST /api/imports", handler.upload)
	mux.HandleFunc("POST /api/deploy", handler.startDeploy)
	mux.HandleFunc("POST /api/deploy/overwrite-preview", handler.previewOverwrite)
	mux.HandleFunc("GET /api/jobs/{id}", handler.jobDetail)
	mux.HandleFunc("POST /api/jobs/{id}/retry", handler.retryDeploy)
	mux.HandleFunc("GET /api/jobs/{id}/download", handler.download)
	mux.HandleFunc("GET /api/", http.NotFound)
}

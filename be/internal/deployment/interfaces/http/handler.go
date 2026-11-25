package httpapi

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"app-deployment-platform/internal/deployment/application"
	"app-deployment-platform/internal/deployment/domain"
	"app-deployment-platform/internal/deployment/infrastructure/artifact"
	"app-deployment-platform/internal/startup"
)

type Handler struct {
	service  *application.Service
	identity startup.Health
}

func NewHandler(service *application.Service, identity startup.Health) *Handler {
	return &Handler{service: service, identity: identity}
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, h.identity)
}

func (h *Handler) cliStatus(w http.ResponseWriter, r *http.Request) {
	binary, err := h.service.Lark.Binary()
	if err != nil {
		writeJSON(w, 200, map[string]any{"installed": false, "profiles": []any{}})
		return
	}
	profiles, err := h.service.Profiles(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{"installed": true, "binary": binary, "platform": runtime.GOOS, "profiles": []any{}, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"installed": true, "binary": binary, "platform": runtime.GOOS, "profiles": profiles})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var input profileRequest
	if err := readJSON(w, r, &input); err != nil {
		failJSON(w, 400, err)
		return
	}
	authorization, status, err := h.service.Auth.Begin(r.Context(), input.Profile)
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, authorization)
}

func (h *Handler) loginStatus(w http.ResponseWriter, r *http.Request) {
	authorization, err := h.service.Auth.Status(r.PathValue("id"), r.URL.Query().Get("profile"))
	if err != nil {
		failJSON(w, 404, err)
		return
	}
	writeJSON(w, 200, authorization)
}

func (h *Handler) refreshLogin(w http.ResponseWriter, r *http.Request) {
	var input profileRequest
	if err := readJSON(w, r, &input); err != nil {
		failJSON(w, 400, err)
		return
	}
	authorization, status, err := h.service.Auth.Refresh(r.Context(), r.PathValue("id"), input.Profile)
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, authorization)
}

func (h *Handler) createTenant(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &input); err != nil {
		failJSON(w, 400, err)
		return
	}
	task, status, err := h.service.Config.Begin(r.Context(), input.Name)
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, task)
}

func (h *Handler) currentTenant(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, h.service.Config.Current())
}

func (h *Handler) initializeTenant(w http.ResponseWriter, r *http.Request) {
	task, status, err := h.service.Config.Initialize(r.Context())
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, task)
}

func (h *Handler) tenantStatus(w http.ResponseWriter, r *http.Request) {
	task, err := h.service.Config.Status(r.PathValue("id"))
	if err != nil {
		failJSON(w, 404, err)
		return
	}
	writeJSON(w, 200, task)
}

func (h *Handler) refreshTenant(w http.ResponseWriter, r *http.Request) {
	task, status, err := h.service.Config.Refresh(r.Context(), r.PathValue("id"))
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, task)
}

func (h *Handler) cancelTenant(w http.ResponseWriter, r *http.Request) {
	task, err := h.service.Config.Cancel(r.PathValue("id"))
	if err != nil {
		failJSON(w, 404, err)
		return
	}
	writeJSON(w, 200, task)
}

func (h *Handler) apps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	query := r.URL.Query()
	result, status, err := h.service.ListApps(ctx, query.Get("profile"), query.Get("keyword"), query.Get("cursor"))
	if err != nil {
		failJSON(w, status, err)
		return
	}
	items := []domain.App{}
	// 仅向页面返回全栈应用。
	for _, app := range result.Items {
		if app.Type == "full_stack" {
			items = append(items, app)
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "cursor": result.NextCursor()})
}

func (h *Handler) jobs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	items, err := h.service.ListJobs(query.Get("kind"), query.Get("profile"))
	if err != nil {
		failJSON(w, 500, errors.New("无法读取本地任务"))
		return
	}
	writeJSON(w, 200, items)
}

func (h *Handler) jobDetail(w http.ResponseWriter, r *http.Request) {
	job, err := h.service.GetJob(r.PathValue("id"), r.URL.Query().Get("profile"))
	if err != nil {
		failJSON(w, 404, err)
		return
	}
	writeJSON(w, 200, job)
}

func (h *Handler) startPackage(w http.ResponseWriter, r *http.Request) {
	var input packageRequest
	if err := readJSON(w, r, &input); err != nil {
		failJSON(w, 400, err)
		return
	}
	job, status, err := h.service.StartPackage(r.Context(), input.Profile, input.AppID)
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, job)
}

func (h *Handler) startDeploy(w http.ResponseWriter, r *http.Request) {
	var input deployRequest
	if err := readJSON(w, r, &input); err != nil {
		failJSON(w, 400, err)
		return
	}
	var job domain.Job
	var status int
	var err error
	if strings.TrimSpace(input.TargetURL) != "" || input.ConfirmedID != "" || input.Confirmation != "" {
		job, status, err = h.service.StartOverwrite(r.Context(), input.Profile, input.ImportID, input.TargetURL, input.ConfirmedID, input.Confirmation)
	} else {
		job, status, err = h.service.StartDeploy(r.Context(), input.Profile, input.ImportID, input.Name)
	}
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, job)
}

func (h *Handler) previewOverwrite(w http.ResponseWriter, r *http.Request) {
	var input deployRequest
	if err := readJSON(w, r, &input); err != nil {
		failJSON(w, 400, err)
		return
	}
	preview, status, err := h.service.PreviewOverwrite(r.Context(), input.Profile, input.ImportID, input.TargetURL)
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, preview)
}

func (h *Handler) retryDeploy(w http.ResponseWriter, r *http.Request) {
	job, status, err := h.service.RetryDeploy(r.Context(), r.PathValue("id"), r.URL.Query().Get("profile"))
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, job)
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, artifact.MaxUpload+(1<<20))
	multipart, err := r.MultipartReader()
	if err != nil {
		failJSON(w, 400, errors.New("请选择 ZIP 安装包"))
		return
	}
	part, err := multipart.NextPart()
	if err != nil || part.FormName() != "file" || !strings.EqualFold(filepath.Ext(part.FileName()), ".zip") {
		failJSON(w, 400, errors.New("请选择 ZIP 安装包"))
		return
	}
	defer part.Close()
	item, status, err := h.service.ImportPackage(part.FileName(), part)
	if err != nil {
		failJSON(w, status, err)
		return
	}
	writeJSON(w, status, item)
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	job, path, err := h.service.PackagePath(r.PathValue("id"), r.URL.Query().Get("profile"))
	if err != nil {
		failJSON(w, 404, err)
		return
	}
	name := strings.Map(func(char rune) rune {
		if char == '/' || char == '\\' || char < 32 {
			return '_'
		}
		return char
	}, job.Name)
	packagedAt := job.UpdatedAt
	if job.FinishedAt != nil && !job.FinishedAt.IsZero() {
		packagedAt = *job.FinishedAt
	}
	if packagedAt.IsZero() {
		packagedAt = job.CreatedAt
	}
	filename := fmt.Sprintf("%s-%s.zip", name, packagedAt.Format("20060102_150405"))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", "application/zip")
	http.ServeFile(w, r, path)
}

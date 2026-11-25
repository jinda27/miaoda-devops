package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"app-deployment-platform/internal/deployment/domain"
	"app-deployment-platform/internal/deployment/infrastructure/artifact"
	"app-deployment-platform/internal/deployment/infrastructure/executor"
	gitinfra "app-deployment-platform/internal/deployment/infrastructure/git"
	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
	"app-deployment-platform/internal/deployment/infrastructure/miaoda"
	"app-deployment-platform/internal/deployment/infrastructure/persistence"
)

type Service struct {
	Root    string
	Jobs    *persistence.FileJobRepository
	Archive *artifact.ZipArchive
	Lark    *larkcli.Client
	Auth    *larkcli.AuthService
	Config  *larkcli.ConfigService
	Miaoda  *miaoda.Client
	Runner  *executor.Runner
	mu      sync.Mutex
	worker  sync.Mutex
}

func NewService(root string) (*Service, error) {
	jobs, err := persistence.NewFileJobRepository(root)
	if err != nil {
		return nil, err
	}
	lark := larkcli.NewClient(root)
	gitRepository := gitinfra.NewRepository()
	service := &Service{
		Root: root, Jobs: jobs, Archive: artifact.NewZipArchive(), Lark: lark,
		Auth: larkcli.NewAuthService(lark), Config: larkcli.NewConfigService(lark), Runner: executor.NewRunner(),
	}
	service.Miaoda = miaoda.NewClient(root, lark, jobs, gitRepository)
	if err := service.interruptJobs(); err != nil {
		return nil, err
	}
	log.Printf("[service] initialized data_dir=%q", root)
	return service, nil
}

func (s *Service) Profiles(ctx context.Context) ([]larkcli.Profile, error) {
	return s.Lark.Profiles(ctx)
}

func (s *Service) SelectProfile(ctx context.Context, name string, ready bool) (larkcli.Profile, error) {
	return s.Lark.SelectProfile(ctx, name, ready)
}

func (s *Service) ListApps(ctx context.Context, profile, keyword, cursor string) (miaoda.AppList, int, error) {
	if strings.TrimSpace(keyword) == "" {
		return miaoda.AppList{Items: []domain.App{}}, 200, nil
	}
	selected, err := s.Lark.SelectProfile(ctx, profile, true)
	if err != nil {
		return miaoda.AppList{}, 401, err
	}
	result, err := s.Miaoda.ListApps(ctx, selected.Name, strings.TrimSpace(keyword), cursor, "full_stack")
	if err != nil {
		return miaoda.AppList{}, 502, err
	}
	return result, 200, nil
}

func (s *Service) ListJobs(kind, profile string) ([]domain.Job, error) {
	items, err := s.Jobs.List()
	if err != nil {
		return nil, err
	}
	result := []domain.Job{}
	// 仅返回当前账号和任务类型的数据。
	for _, job := range items {
		if job.Kind == kind && profile != "" && job.Profile == profile {
			result = append(result, job)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result, nil
}

func (s *Service) GetJob(id, profile string) (domain.Job, error) {
	return s.Jobs.Get(id, profile)
}

func (s *Service) StartPackage(ctx context.Context, profile, appID string) (domain.Job, int, error) {
	if !domain.AppIDPattern.MatchString(appID) {
		return domain.Job{}, 400, errors.New("请选择要打包的妙搭应用")
	}
	selected, err := s.Lark.SelectProfile(ctx, profile, true)
	if err != nil {
		return domain.Job{}, 401, err
	}
	job := domain.Job{
		Kind: "package", Profile: selected.Name, Name: appID, Source: domain.App{ID: appID},
		Steps: domain.NewSteps("检查应用权限", "获取应用源码", "导出部署配置", "生成安装包", "校验安装包"),
	}
	return s.newJob(job)
}

func (s *Service) StartDeploy(ctx context.Context, profile, importID, name string) (domain.Job, int, error) {
	imported, status, err := s.deploymentPackage(importID)
	if err != nil {
		return domain.Job{}, status, err
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) == 0 || len([]rune(name)) > 100 {
		return domain.Job{}, 400, errors.New("应用名称须为 1 至 100 个字符")
	}
	selected, err := s.Lark.SelectProfile(ctx, profile, true)
	if err != nil {
		return domain.Job{}, 401, err
	}
	labels := []string{"校验并解压安装包", "创建/复用应用", "提交并推送代码", "发布上线"}
	if imported.Meta.App.Type == "full_stack" {
		labels = append([]string{"校验并解压安装包"}, domain.FullDeploySteps...)
	}
	job := domain.Job{
		Kind: "deploy", Profile: selected.Name, Name: name, Source: imported.Meta.App,
		ImportID: importID, Steps: domain.NewSteps(labels...),
	}
	return s.newJob(job)
}

func (s *Service) deploymentPackage(importID string) (domain.Attachment, int, error) {
	var imported domain.Attachment
	if !domain.ObjectIDPattern.MatchString(importID) {
		return imported, 400, errors.New("请先导入有效的安装包")
	}
	if persistence.ReadJSON(filepath.Join(s.Jobs.ImportDir(importID), "import.json"), &imported) != nil {
		return imported, 404, errors.New("找不到导入的安装包")
	}
	return imported, 200, nil
}

func (s *Service) newJob(job domain.Job) (domain.Job, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.Kind == "deploy" {
		existing, err := s.Jobs.List()
		if err != nil {
			return domain.Job{}, 500, errors.New("无法读取本地任务")
		}
		// 相同安装包与名称复用既有任务。
		for _, item := range existing {
			if job.Overwrite != nil {
				if item.Kind == job.Kind && item.TargetID == job.TargetID {
					if item.Profile == job.Profile && item.ImportID == job.ImportID &&
						item.OverwriteConfirmation == job.OverwriteConfirmation {
						return item, 200, nil
					}
					if item.Status == domain.JobQueued || item.Status == domain.JobRunning {
						return domain.Job{}, 409, errors.New("目标应用已有进行中的部署任务")
					}
				}
				continue
			}
			if item.Kind == job.Kind && item.Profile == job.Profile &&
				item.ImportID == job.ImportID && item.Name == job.Name && item.Overwrite == nil {
				log.Printf("[job] reused job_id=%s kind=%s profile=%q import_id=%s status=%s",
					item.ID, item.Kind, item.Profile, item.ImportID, item.Status)
				return item, 200, nil
			}
		}
	}
	now := time.Now()
	job.ID, job.Status, job.CreatedAt, job.UpdatedAt = newID(), domain.JobQueued, now, now
	job.QueuedAt = &now
	if err := os.Mkdir(s.Jobs.JobDir(job.ID), 0700); err != nil {
		return domain.Job{}, 500, errors.New("无法保存本地任务")
	}
	if err := persistence.WriteJSON(filepath.Join(s.Jobs.JobDir(job.ID), "job.json"), &job); err != nil {
		return domain.Job{}, 500, errors.New("无法保存本地任务")
	}
	log.Printf("[job] queued job_id=%s kind=%s profile=%q app_type=%s import_id=%s steps=%d",
		job.ID, job.Kind, job.Profile, job.Source.Type, job.ImportID, len(job.Steps))
	go s.runJob(job)
	return job, 202, nil
}

func (s *Service) runJob(job domain.Job) {
	s.worker.Lock()
	defer s.worker.Unlock()
	started := time.Now()
	job.Start(started)
	log.Printf("[job] started job_id=%s kind=%s resume_from=%q queue_duration_ms=%d total_duration_ms=%d",
		job.ID, job.Kind, job.ResumeFrom, job.QueueDurationMs, job.DurationMs)
	if err := s.Jobs.Save(&job); err != nil {
		log.Printf("[job] persist_failed job_id=%s phase=start error=%q", job.ID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	var err error
	if job.Kind == "package" {
		err = s.packageJob(ctx, &job)
	} else {
		err = s.deployJob(ctx, &job)
	}
	job.Finish(err)
	// 输出各步骤的最终耗时，便于比较瓶颈。
	for _, step := range job.Steps {
		log.Printf("[timing] job_id=%s key=%q status=%s duration_ms=%d recorded=%t incomplete=%t estimated=%t",
			job.ID, step.Key, step.Status, step.DurationMs, step.StartedAt != nil || step.FinishedAt != nil,
			step.TimingIncomplete, step.TimingEstimated)
	}
	if saveErr := s.Jobs.Save(&job); saveErr != nil {
		log.Printf("[job] persist_failed job_id=%s phase=finish error=%q", job.ID, saveErr)
	}
	if err != nil {
		log.Printf("[job] finished job_id=%s status=%s duration_ms=%d total_duration_ms=%d queue_duration_ms=%d target_id=%s error=%q",
			job.ID, job.Status, time.Since(started).Milliseconds(), job.DurationMs, job.QueueDurationMs, job.TargetID, err)
		return
	}
	log.Printf("[job] finished job_id=%s status=%s duration_ms=%d total_duration_ms=%d queue_duration_ms=%d target_id=%s release_id=%s",
		job.ID, job.Status, time.Since(started).Milliseconds(), job.DurationMs, job.QueueDurationMs, job.TargetID, job.ReleaseID)
}

func (s *Service) deployJob(ctx context.Context, job *domain.Job) error {
	const validate = "校验并解压安装包"
	resuming := job.ResumeFrom != ""
	if !resuming {
		s.Miaoda.SetStep(job, validate, domain.StepRunning, "")
	}
	filename := filepath.Join(s.Jobs.ImportDir(job.ImportID), "package.zip")
	log.Printf("[job] package_validate job_id=%s import_id=%s resume=%t", job.ID, job.ImportID, resuming)
	if _, err := s.Archive.Inspect(filename); err != nil {
		if resuming {
			s.Miaoda.SetStep(job, validate, domain.StepRunning, "")
		}
		return err
	}
	work, err := os.MkdirTemp(s.Jobs.JobDir(job.ID), "deploy-")
	if err != nil {
		return errors.New("无法创建部署工作目录")
	}
	defer os.RemoveAll(work)
	source := filepath.Join(work, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		return err
	}
	if err := s.Archive.Extract(filename, source); err != nil {
		return err
	}
	log.Printf("[job] package_extracted job_id=%s app_type=%s work_dir=%q", job.ID, job.Source.Type, work)
	if !resuming {
		s.Miaoda.SetStep(job, validate, domain.StepSuccess, "")
	}
	if job.Source.Type == "full_stack" {
		// 清理源应用身份后进入全栈部署。
		for _, name := range []string{".spark", ".spark_project"} {
			if err := os.RemoveAll(filepath.Join(source, name)); err != nil {
				return errors.New("无法清理源应用的身份配置")
			}
		}
		return s.deployFullStack(ctx, job, work, source)
	}
	return s.Miaoda.DeploySource(ctx, job, work, source)
}

func (s *Service) interruptJobs() error {
	jobs, err := s.Jobs.List()
	if err != nil {
		return err
	}
	// 将进程重启前的运行中任务标记为失败。
	for _, job := range jobs {
		if job.Status == domain.JobRunning || job.Status == domain.JobQueued {
			previousStatus := job.Status
			s.readDeployState(&job)
			// 中断时间按最后快照估算，避免把停机时间计入耗时。
			job.TimingEstimated = job.QueuedAt != nil || job.StartedAt != nil
			for i := range job.Steps {
				if job.Steps[i].Status == domain.StepRunning && job.Steps[i].StartedAt != nil {
					job.Steps[i].TimingEstimated = true
				}
			}
			job.FinishAt(errors.New("服务已重启，任务执行中断"), job.UpdatedAt)
			if err := s.Jobs.Save(&job); err != nil {
				return err
			}
			log.Printf("[job] interrupted job_id=%s previous_status=%s target_id=%s",
				job.ID, previousStatus, job.TargetID)
		}
	}
	return nil
}

func (s *Service) readDeployState(job *domain.Job) {
	if job.Kind != "deploy" || job.Source.Type != "full_stack" {
		return
	}
	var state struct {
		AppID string `json:"app_id"`
	}
	if persistence.ReadJSON(filepath.Join(s.Jobs.JobDir(job.ID), "deploy-state.json"), &state) == nil &&
		domain.AppIDPattern.MatchString(state.AppID) {
		job.TargetID = state.AppID
		log.Printf("[job] deploy_state_restored job_id=%s target_id=%s", job.ID, state.AppID)
	}
}

func newID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(random[:])
}

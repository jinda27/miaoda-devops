package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"app-deployment-platform/internal/environment/domain"
	"app-deployment-platform/internal/environment/infrastructure"
)

type Tools interface {
	Inspect(context.Context) domain.Report
	Install(context.Context, string, func(string, int)) error
}

type Service struct {
	tools Tools
	mu    sync.Mutex
	job   *domain.Installation
}

func NewService(tools *infrastructure.Toolset) *Service {
	return &Service{tools: tools}
}

func (s *Service) Status(ctx context.Context) domain.Report {
	report := s.tools.Inspect(ctx)
	report.Installation = s.Installation()
	return report
}

func (s *Service) Installation() *domain.Installation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil
	}
	copy := *s.job
	copy.Steps = append([]domain.Step(nil), s.job.Steps...)
	return &copy
}

func (s *Service) Start(ctx context.Context) (*domain.Installation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 同一时刻只运行一个安装任务，多次点击复用任务。
	if s.job != nil && s.job.Status == "running" {
		copy := *s.job
		copy.Steps = append([]domain.Step(nil), s.job.Steps...)
		return &copy, nil
	}
	report := s.tools.Inspect(ctx)
	if !report.Supported {
		return nil, errors.New("当前仅支持 Apple Silicon / Intel Mac 和 Windows x64")
	}
	steps := installPlan(report)
	if len(steps) == 0 {
		return nil, errors.New("环境已就绪，无需安装")
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	s.job = &domain.Installation{ID: hex.EncodeToString(id), Status: "running",
		Steps: steps, StartedAt: time.Now()}
	copy := *s.job
	copy.Steps = append([]domain.Step(nil), s.job.Steps...)
	go s.run()
	return &copy, nil
}

func installPlan(report domain.Report) []domain.Step {
	items := make(map[string]domain.Item, len(report.Items))
	for _, item := range report.Items {
		items[item.ID] = item
	}
	plan := []struct {
		component string
		name      string
		ids       []string
	}{
		{"lark-cli", "lark-cli", []string{"lark-cli"}},
		{"node", "Node.js / npm", []string{"node", "npm"}},
		{"git", "Git", []string{"git"}},
		{"zip", "zip", []string{"zip"}},
		{"assemble", "打包组件", []string{"assemble"}},
	}
	if report.Platform == "windows" {
		plan[2].name, plan[2].ids = "Git / Bash", []string{"git", "bash"}
	} else {
		plan = append(plan, struct {
			component string
			name      string
			ids       []string
		}{"bash", "Bash", []string{"bash"}})
	}
	var steps []domain.Step
	// 合并共享安装包的依赖，只安装未就绪项。
	for _, entry := range plan {
		for _, id := range entry.ids {
			if item, exists := items[id]; exists && item.Status != domain.Ready {
				steps = append(steps, domain.Step{ID: entry.component, Name: entry.name, Status: "pending"})
				break
			}
		}
	}
	return steps
}

func (s *Service) run() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	snapshot := s.Installation()
	failed := 0
	// 独立组件继续安装，重试时只处理仍未就绪的组件。
	for index, step := range snapshot.Steps {
		s.update(index, "running", "准备安装", "", index*100/len(snapshot.Steps))
		log.Printf("[environment] install_started component=%s", step.ID)
		err := s.tools.Install(ctx, step.ID, func(phase string, percent int) {
			s.update(index, "running", phase, "", (index*100+percent)/len(snapshot.Steps))
		})
		if err != nil {
			failed++
			s.update(index, "failed", "", err.Error(), (index+1)*100/len(snapshot.Steps))
			log.Printf("[environment] install_failed component=%s error=%q", step.ID, err)
			continue
		}
		s.update(index, "success", "已完成", "", (index+1)*100/len(snapshot.Steps))
		log.Printf("[environment] install_completed component=%s", step.ID)
	}
	report := s.tools.Inspect(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.job.Percent, s.job.FinishedAt = 100, time.Now()
	// 最终以真实复检结果决定是否允许进入账号连接。
	if failed == 0 && report.Ready {
		s.job.Status, s.job.Message = "success", "环境已就绪"
	} else {
		s.job.Status, s.job.Message = "failed", fmt.Sprintf("仍有 %d 项依赖未就绪，请重试或查看对应提示", missingCount(report))
	}
}

func missingCount(report domain.Report) int {
	count := 0
	for _, item := range report.Items {
		if item.Status != domain.Ready {
			count++
		}
	}
	return count
}

func (s *Service) update(index int, status, phase, message string, percent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.job.Steps[index].Status, s.job.Steps[index].Phase, s.job.Steps[index].Message = status, phase, message
	s.job.Percent = percent
}

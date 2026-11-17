package application

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"app-deployment-platform/internal/environment/domain"
)

type fakeTools struct {
	mu        sync.Mutex
	ready     map[string]bool
	calls     map[string]int
	failOnce  string
	block     chan struct{}
	supported bool
}

func (f *fakeTools) Inspect(context.Context) domain.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	report := domain.Report{Platform: "darwin", Supported: f.supported, Ready: true}
	for _, id := range []string{"node", "npm", "lark-cli", "git", "bash", "zip", "assemble"} {
		status := domain.Ready
		if !f.ready[id] {
			status, report.Ready = domain.Missing, false
		}
		report.Items = append(report.Items, domain.Item{ID: id, Status: status})
	}
	return report
}

func (f *fakeTools) Install(_ context.Context, component string, progress func(string, int)) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[component]++
	if f.failOnce == component && f.calls[component] == 1 {
		return errors.New("fixture failure")
	}
	f.ready[component] = true
	if component == "node" {
		f.ready["npm"] = true
	}
	progress("已完成", 100)
	return nil
}

func newFake() *fakeTools {
	return &fakeTools{supported: true, calls: map[string]int{},
		ready: map[string]bool{"git": true, "bash": true, "zip": true, "assemble": true}}
}

func awaitInstallation(t *testing.T, service *Service) *domain.Installation {
	t.Helper()
	deadline, ticker := time.NewTimer(time.Second), time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("installation did not finish")
		case <-ticker.C:
			job := service.Installation()
			if job.Status != "running" {
				return job
			}
		}
	}
}

func TestInstallOnlyMissingAndRetryFailure(t *testing.T) {
	tools := newFake()
	tools.failOnce = "lark-cli"
	service := &Service{tools: tools}
	first, err := service.Start(context.Background())
	if err != nil || len(first.Steps) != 2 {
		t.Fatalf("unexpected plan: %+v %v", first, err)
	}
	if result := awaitInstallation(t, service); result.Status != "failed" {
		t.Fatalf("failure not reported: %+v", result)
	}
	retry, err := service.Start(context.Background())
	if err != nil || len(retry.Steps) != 1 || retry.Steps[0].ID != "lark-cli" {
		t.Fatalf("retry includes ready dependencies: %+v %v", retry, err)
	}
	if result := awaitInstallation(t, service); result.Status != "success" {
		t.Fatalf("retry did not succeed: %+v", result)
	}
	tools.mu.Lock()
	defer tools.mu.Unlock()
	if tools.calls["node"] != 1 || tools.calls["lark-cli"] != 2 || len(tools.calls) != 2 {
		t.Fatalf("unexpected install counts: %v", tools.calls)
	}
}

func TestConcurrentStartReusesRunningInstallation(t *testing.T) {
	tools := newFake()
	tools.block = make(chan struct{})
	service := &Service{tools: tools}
	first, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			job, err := service.Start(context.Background())
			if err != nil || job.ID != first.ID {
				t.Errorf("concurrent install was not reused: %+v %v", job, err)
			}
		}()
	}
	group.Wait()
	close(tools.block)
	if result := awaitInstallation(t, service); result.Status != "success" {
		t.Fatalf("unexpected completion: %+v", result)
	}
}

func TestWindowsInstallPlanGroupsGitAndBash(t *testing.T) {
	report := domain.Report{Platform: "windows", Items: []domain.Item{
		{ID: "git", Status: domain.Ready}, {ID: "bash", Status: domain.VersionMismatch},
		{ID: "node", Status: domain.Ready}, {ID: "npm", Status: domain.Missing},
	}}
	plan := installPlan(report)
	if len(plan) != 2 || plan[0].ID != "node" || plan[1].ID != "git" {
		t.Fatalf("unexpected shared-package plan: %+v", plan)
	}
}

func TestCLIInstallsBeforeOtherDownloadDependencies(t *testing.T) {
	// 先准备 CLI，供后续网络失败时读取飞书附件。
	report := newFake().Inspect(context.Background())
	plan := installPlan(report)
	if len(plan) != 2 || plan[0].ID != "lark-cli" || plan[1].ID != "node" {
		t.Fatalf("CLI 没有优先安装：%+v", plan)
	}
}

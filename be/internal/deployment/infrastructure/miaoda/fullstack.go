package miaoda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"app-deployment-platform/internal/deployment/domain"
	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
	"app-deployment-platform/internal/deployment/infrastructure/persistence"
)

const assembleRelativeDir = "docs/agent_assemble"

type fullStackCredentials struct {
	Repo  string `json:"repository_url"`
	Name  string `json:"commit_author_name"`
	Email string `json:"commit_author_email"`
}

// deployFullStackGo 使用 Go 完成全栈应用部署，不依赖 Skill 脚本。
func (s *Client) DeployFullStack(ctx context.Context, j *domain.Job, work, source string) error {
	resumeIndex := domain.FullDeployStepIndex(j.ResumeFrom)
	if j.ResumeFrom == domain.ResetTargetStep {
		resumeIndex = domain.FullDeployStepIndex("提交并推送代码")
	}
	if resumeIndex < 0 {
		resumeIndex = 0
	}
	shouldRun := func(key string) bool {
		return domain.FullDeployStepIndex(key) >= resumeIndex
	}
	codeStage := resumeIndex <= domain.FullDeployStepIndex("提交并推送代码")

	var roles []roleDefinition
	var adminID, deployerOpenID string
	// 在创建应用、推送代码和清理目标前检查角色成员身份。
	if shouldRun("创建应用角色") {
		var err error
		roles, adminID, deployerOpenID, err = s.prepareDeployRoles(ctx, j.Profile, work, source)
		if err != nil {
			key := j.ResumeFrom
			// 续跑保留原中断点，避免失败重试重新执行已完成的写操作。
			if domain.StepIndex(j.Steps, key) < 0 || key == "校验并解压安装包" {
				key = "创建/复用应用"
			}
			s.SetStep(j, key, domain.StepRunning, "检查部署者身份")
			return fmt.Errorf("部署前角色检查失败：%w", err)
		}
	}

	// 创建或校验目标应用。
	if shouldRun("创建/复用应用") {
		s.SetStep(j, "创建/复用应用", "running", "")
		if err := s.ensureFullStackApp(ctx, j, work); err != nil {
			return err
		}
		s.SetStep(j, "创建/复用应用", "success", j.TargetID)
	}
	if !domain.AppIDPattern.MatchString(j.TargetID) {
		return errors.New("断点续跑缺少有效的目标应用 ID")
	}

	var repo string
	var credentials fullStackCredentials
	// 拉取代码并覆盖安装包内容。
	if codeStage {
		s.SetStep(j, "拉取应用代码", "running", "")
		var err error
		repo, credentials, err = s.cloneFullStackRepo(ctx, j, work)
		if err != nil {
			return err
		}
		s.SetStep(j, "拉取应用代码", "success", "")
	}
	if codeStage {
		if repo == "" {
			return errors.New("缺少目标应用代码目录")
		}
		s.SetStep(j, "覆盖工程目录", "running", "")
		if err := s.Git.ReplaceSource(ctx, source, repo); err != nil {
			return fmt.Errorf("覆盖工程目录失败：%w", err)
		}
		if err := protectDeploymentDocuments(repo); err != nil {
			return err
		}
		if _, err := readPackageJSON(repo); err != nil {
			return err
		}
		count, err := fixPackageHooks(repo)
		if err != nil {
			return err
		}
		detail := "工程目录已覆盖，package.json 校验通过"
		if count > 0 {
			detail += fmt.Sprintf("，已移除 %d 个无效钩子", count)
		}
		s.SetStep(j, "覆盖工程目录", "success", detail)
	}
	if codeStage {
		if j.Overwrite != nil && j.ResumeFrom != "提交并推送代码" {
			s.SetStep(j, domain.ResetTargetStep, domain.StepRunning, "")
			if _, err := s.Git.Command(ctx, repo, "push", "--dry-run", "origin", Branch); err != nil {
				return fmt.Errorf("无法确认目标仓库推送权限：%w", err)
			}
			if err := s.ResetOverwriteTarget(ctx, j, work, source); err != nil {
				return err
			}
			s.SetStep(j, domain.ResetTargetStep, domain.StepSuccess, "已清理目标应用")
		}
		s.SetStep(j, "提交并推送代码", "running", "")
		if err := s.commitAndPushFullStack(ctx, repo, credentials); err != nil {
			return err
		}
		s.SetStep(j, "提交并推送代码", "success", "")
	}

	// DDL 失败必须停止，不能继续创建发布。
	if shouldRun("执行 DDL") {
		s.SetStep(j, "执行 DDL", "running", "")
		if err := s.runDDL(ctx, j, source, work); err != nil {
			return err
		}
		s.SetStep(j, "执行 DDL", "success", "")
	}
	if shouldRun("创建应用角色") {
		if err := s.deployRoles(ctx, j, work, roles, adminID, deployerOpenID); err != nil {
			return err
		}
	}
	if shouldRun("创建自动化") {
		if err := s.deployAutomations(ctx, j, work, source); err != nil {
			return err
		}
	}
	if shouldRun("设置环境变量") {
		if err := s.deployEnvironment(ctx, j, work, source); err != nil {
			return err
		}
	}
	if shouldRun("发布上线") {
		if err := s.releaseFullStack(ctx, j, work); err != nil {
			return err
		}
	}
	j.Message = "请在妙搭后台核对发布后的应用可见范围和配置。"
	return nil
}

// ensureFullStackApp 创建或校验目标应用。
func (s *Client) ensureFullStackApp(ctx context.Context, j *domain.Job, work string) error {
	if j.TargetID != "" {
		var detail map[string]any
		if err := s.Request(ctx, j.Profile, work, "GET", Path(j.TargetID, ""), nil, nil, &detail); err != nil {
			return fmt.Errorf("目标应用无法访问：%w", err)
		}
		return s.writeDeployState(j, "reuse")
	}
	if err := s.checkTargetName(ctx, j); err != nil {
		return err
	}
	var created map[string]any
	err := s.Request(ctx, j.Profile, work, "POST", "/open-apis/spark/v1/apps", nil, map[string]any{
		"app_type":    "full_stack",
		"name":        j.Name,
		"description": j.Source.Description,
	}, &created)
	if err == nil {
		j.TargetID = nestedString(created, "app_id")
		if j.TargetID == "" {
			if app, ok := created["app"].(map[string]any); ok {
				j.TargetID = nestedString(app, "app_id")
			}
		}
	}
	// 创建结果未知时按名称回查，避免生成重复应用。
	if err != nil || !domain.AppIDPattern.MatchString(j.TargetID) {
		items, queryErr := s.exactApps(ctx, j.Profile, j.Name)
		if queryErr == nil && len(items) == 1 {
			j.TargetID = items[0].ID
		} else if err != nil {
			return fmt.Errorf("创建应用失败，且无法按名称确认创建结果：%w", err)
		} else {
			return errors.New("妙搭未返回有效的目标应用 ID")
		}
	}
	if err := s.Jobs.Save(j); err != nil {
		return errors.New("应用已创建，但无法保存目标应用 ID")
	}
	return s.writeDeployState(j, "create")
}

// exactApps 精确查询同名应用。
func (s *Client) exactApps(ctx context.Context, profile, name string) ([]domain.App, error) {
	result, err := s.ListApps(ctx, profile, name, "", "")
	if err != nil {
		return nil, err
	}
	items := []domain.App{}
	for _, item := range result.Items {
		if strings.TrimSpace(item.Name) == strings.TrimSpace(name) {
			items = append(items, item)
		}
	}
	return items, nil
}

// writeDeployState 持久化目标应用，供断点续跑使用。
func (s *Client) writeDeployState(j *domain.Job, mode string) error {
	state := map[string]any{
		"app_id": j.TargetID, "app_name": j.Name, "mode": mode,
		"branch": Branch, "result": "running", "updated_at": time.Now().Format(time.RFC3339),
	}
	if err := persistence.WriteJSON(filepath.Join(s.Jobs.JobDir(j.ID), "deploy-state.json"), state); err != nil {
		return errors.New("无法保存部署断点状态")
	}
	return nil
}

// cloneFullStackRepo 初始化凭据并拉取目标仓库。
func (s *Client) cloneFullStackRepo(ctx context.Context, j *domain.Job, work string) (string, fullStackCredentials, error) {
	var result struct {
		Data fullStackCredentials `json:"data"`
	}
	if err := s.Lark.Run(ctx, work, j.Profile, &result, "apps", "+git-credential-init",
		"--app-id", j.TargetID, "--as", "user", "--json"); err != nil {
		return "", fullStackCredentials{}, err
	}
	credentials := result.Data
	address, err := url.Parse(credentials.Repo)
	if err != nil || address.Scheme != "https" || address.Hostname() == "" || address.User != nil {
		return "", credentials, errors.New("妙搭未返回有效的 Git 仓库地址")
	}
	target := filepath.Join(work, "repo")
	_, err = s.Git.Command(ctx, work, "clone", "--depth=1", "--single-branch", "--branch", Branch, "--", credentials.Repo, target)
	if err != nil {
		// 新应用可能尚无目标分支，回退到默认分支。
		_ = os.RemoveAll(target)
		if _, err = s.Git.Command(ctx, work, "clone", "--depth=1", "--", credentials.Repo, target); err != nil {
			return "", credentials, err
		}
	}
	if _, err := s.Git.Command(ctx, target, "checkout", Branch); err != nil {
		if _, err := s.Git.Command(ctx, target, "checkout", "-b", Branch); err != nil {
			return "", credentials, err
		}
	}
	return target, credentials, nil
}

// protectDeploymentDocuments 避免部署配置进入目标仓库。
func protectDeploymentDocuments(repo string) error {
	path := filepath.Join(repo, ".gitignore")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("无法读取目标工程 .gitignore")
	}
	if strings.Contains(string(raw), "/docs/agent_assemble/") {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("无法保护部署配置文件")
	}
	defer file.Close()
	if _, err := file.WriteString("\n/docs/agent_assemble/\n"); err != nil {
		return errors.New("无法更新目标工程 .gitignore")
	}
	return nil
}

// readPackageJSON 校验 package.json。
func readPackageJSON(repo string) (map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(repo, "package.json"))
	if err != nil {
		return nil, errors.New("安装包缺少 package.json")
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil, errors.New("package.json 不是有效 JSON")
	}
	return value, nil
}

var nodeHookPattern = regexp.MustCompile(`^\s*node\s+([^\s]+\.js)\s*$`)

// fixPackageHooks 删除引用不存在脚本的生命周期钩子。
func fixPackageHooks(repo string) (int, error) {
	pkg, err := readPackageJSON(repo)
	if err != nil {
		return 0, err
	}
	scripts, _ := pkg["scripts"].(map[string]any)
	count := 0
	// 仅检查会影响安装、构建和启动的钩子。
	for _, name := range []string{"preinstall", "postinstall", "prebuild", "postbuild", "prestart", "poststart"} {
		command, _ := scripts[name].(string)
		match := nodeHookPattern.FindStringSubmatch(command)
		if len(match) != 2 {
			continue
		}
		if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(match[1]))); errors.Is(err, os.ErrNotExist) {
			delete(scripts, name)
			count++
		}
	}
	if count == 0 {
		return 0, nil
	}
	pkg["scripts"] = scripts
	raw, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return 0, errors.New("无法序列化 package.json")
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(repo, "package.json"), raw, 0600); err != nil {
		return 0, errors.New("无法保存 package.json")
	}
	return count, nil
}

// commitAndPushFullStack 提交并推送覆盖后的工程。
func (s *Client) commitAndPushFullStack(ctx context.Context, repo string, credentials fullStackCredentials) error {
	if _, err := s.Git.Command(ctx, repo, "add", "-A"); err != nil {
		return err
	}
	changed, err := s.Git.Command(ctx, repo, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(changed) != "" {
		name, email := credentials.Name, credentials.Email
		if name == "" {
			name = "Miaoda Deployment"
		}
		if email == "" {
			email = "deployment@noreply"
		}
		if _, err := s.Git.Command(ctx, repo, "-c", "user.name="+name, "-c", "user.email="+email,
			"commit", "-m", "Deploy Miaoda package"); err != nil {
			return err
		}
	}
	var pushErr error
	// 瞬时网关错误允许重试同一提交。
	for attempt := 0; attempt < 3; attempt++ {
		if _, pushErr = s.Git.Command(ctx, repo, "push", "origin", Branch); pushErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return pushErr
}

func (s *Client) prepareDeployRoles(ctx context.Context, profile, work, source string) ([]roleDefinition, string, string, error) {
	roles, adminID, err := parseRolesDocument(filepath.Join(source, assembleRelativeDir, "roles.md"))
	if err != nil {
		return nil, "", "", fmt.Errorf("解析角色配置失败：%w", err)
	}
	// 无角色的安装包不需要进行身份查询。
	if len(roles) == 0 {
		return roles, adminID, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	openID, err := s.currentUserOpenID(ctx, profile, work)
	return roles, adminID, openID, err
}

// deployRoles 创建角色并将已确认的部署者加入角色。
func (s *Client) deployRoles(ctx context.Context, j *domain.Job, work string, roles []roleDefinition, adminID, openID string) error {
	const stepName = "创建应用角色"
	s.SetStep(j, stepName, "running", "")
	if len(roles) == 0 {
		s.SetStep(j, stepName, "skipped", "安装包未声明应用角色")
		return nil
	}
	var listed struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.Request(ctx, j.Profile, work, "GET", Path(j.TargetID, "/roles"), nil, nil, &listed); err != nil {
		return fmt.Errorf("查询应用角色失败：%w", err)
	}
	byID, byName := map[string]bool{}, map[string]string{}
	for _, item := range listed.Items {
		id, name := nestedString(item, "role_id"), nestedString(item, "name")
		byID[id], byName[name] = true, id
	}
	// 按编码幂等创建角色。
	for _, role := range roles {
		if byID[role.RoleID] {
			continue
		}
		if existing := byName[role.Name]; existing != "" {
			return fmt.Errorf("角色冲突：%s 已使用编码 %s", role.Name, existing)
		}
		body := map[string]any{"role_id": role.RoleID, "name": role.Name, "description": role.Description}
		if err := s.Request(ctx, j.Profile, work, "POST", Path(j.TargetID, "/roles"), nil, body, nil); err != nil {
			return fmt.Errorf("创建角色 %s 失败：%w", role.Name, err)
		}
		byID[role.RoleID], byName[role.Name] = true, role.RoleID
	}

	memberFailures := []string{}
	adminFailed := false
	// 将部署者加入全部角色。
	for _, role := range roles {
		body := map[string]any{"users": []string{openID}}
		path := Path(j.TargetID, "/roles/"+url.PathEscape(role.RoleID)+"/member_add")
		if err := s.Request(ctx, j.Profile, work, "POST", path, nil, body, nil); err != nil {
			memberFailures = append(memberFailures, fmt.Sprintf("%s（%s）", role.Name, clipText(err.Error(), 160)))
			adminFailed = adminFailed || role.RoleID == adminID
		}
	}
	if len(memberFailures) > 0 {
		detail := "角色已创建，部署者加入以下角色失败：" + strings.Join(memberFailures, "、")
		if adminFailed {
			detail = "管理员角色成员添加失败；" + detail
		}
		s.SetStep(j, stepName, "warning", detail)
		return nil
	}
	s.SetStep(j, stepName, "success", fmt.Sprintf("角色就绪（%d 个）", len(roles)))
	return nil
}

// currentUserOpenID 查询当前 CLI 用户身份。
func (s *Client) currentUserOpenID(ctx context.Context, profile, work string) (string, error) {
	var status struct {
		Identities struct {
			User *struct {
				Available *bool  `json:"available"`
				OpenID    string `json:"openId"`
			} `json:"user"`
		} `json:"identities"`
	}
	// 优先读取同一租户的本地用户态，避免额外申请通讯录权限。
	if err := s.Lark.Run(ctx, work, profile, &status, "auth", "status", "--json"); err == nil {
		if user := status.Identities.User; user != nil && user.Available != nil {
			if !*user.Available {
				return "", errors.New("当前部署账号的飞书用户登录不可用，请到「切换账号」重新授权后重试")
			}
			if validUserOpenID(user.OpenID) {
				return user.OpenID, nil
			}
		}
	}
	// 兼容未返回用户身份字段的 CLI 版本。
	var result struct {
		Data struct {
			User struct {
				OpenID string `json:"open_id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := s.Lark.Run(ctx, work, profile, &result, "contact", "+get-user", "--as", "user", "--json"); err != nil {
		var cliErr *larkcli.CLIError
		if errors.As(err, &cliErr) && (cliErr.Subtype == "missing_scope" || cliErr.Subtype == "insufficient_scope") {
			return "", errors.New("当前部署账号缺少「获取用户基本信息」权限（contact:user.basic_profile:readonly）。请在飞书开放平台为该应用开通权限，再对同一租户账号重新授权后重试")
		}
		return "", fmt.Errorf("读取部署者身份失败：%w", err)
	}
	if !validUserOpenID(result.Data.User.OpenID) {
		return "", errors.New("飞书 CLI 未返回有效的部署者 open_id，请确认使用飞书用户登录，并重新授权后重试")
	}
	return result.Data.User.OpenID, nil
}

func validUserOpenID(id string) bool {
	return len(id) > len("ou_") && strings.HasPrefix(id, "ou_") && len(strings.Fields(id)) == 1 && strings.TrimSpace(id) == id
}

// deployAutomations 创建或更新自动化触发器。
func (s *Client) deployAutomations(ctx context.Context, j *domain.Job, work, source string) error {
	const stepName = "创建自动化"
	s.SetStep(j, stepName, "running", "")
	triggers, err := parseAutomationDocument(filepath.Join(source, assembleRelativeDir, "automation.md"))
	if err != nil {
		return fmt.Errorf("解析自动化配置失败：%w", err)
	}
	if len(triggers) == 0 {
		s.SetStep(j, stepName, "skipped", "安装包未声明自动化")
		return nil
	}
	var listed struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.Request(ctx, j.Profile, work, "GET", Path(j.TargetID, "/triggers"),
		map[string]any{"page_size": 100}, nil, &listed); err != nil {
		s.SetStep(j, stepName, "warning", "自动化列表查询失败，需在妙搭后台手动确认")
		return nil
	}
	existing := map[string]map[string]any{}
	for _, item := range listed.Items {
		if name := nestedString(item, "name"); name != "" {
			existing[name] = item
		}
	}
	created, updated, skipped := 0, 0, 0
	failures := []string{}
	// 同名触发器仅同步描述，新触发器完整创建。
	for _, trigger := range triggers {
		name := nestedString(trigger, "name")
		if current := existing[name]; current != nil {
			expected, actual := nestedString(trigger, "description"), nestedString(current, "description")
			if expected != "" && expected != actual {
				path := Path(j.TargetID, "/triggers/"+url.PathEscape(name))
				if err := s.Request(ctx, j.Profile, work, "PUT", path, nil,
					map[string]any{"description": expected}, nil); err != nil {
					failures = append(failures, name)
					continue
				}
				updated++
			}
			skipped++
			continue
		}
		if err := s.Request(ctx, j.Profile, work, "POST", Path(j.TargetID, "/triggers"), nil, trigger, nil); err != nil {
			failures = append(failures, name)
			continue
		}
		created++
	}
	detail := fmt.Sprintf("创建 %d 个，更新 %d 个，已存在 %d 个", created, updated, skipped)
	if len(failures) > 0 {
		s.SetStep(j, stepName, "warning", detail+"；失败："+strings.Join(failures, "、"))
		return nil
	}
	s.SetStep(j, stepName, "success", detail)
	return nil
}

var reservedEnvKeys = map[string]bool{
	"INTEGRATION_TOKEN": true, "MIAODA_APP_ARCH_TYPE": true, "MIAODA_APP_ID": true,
	"MIAODA_APP_TYPE": true, "SUDA_DATABASE_URL": true,
}

// deployEnvironment 创建或更新应用环境变量。
func (s *Client) deployEnvironment(ctx context.Context, j *domain.Job, work, source string) error {
	const stepName = "设置环境变量"
	s.SetStep(j, stepName, "running", "")
	definitions, err := parseEnvDocument(filepath.Join(source, assembleRelativeDir, "env.md"))
	if err != nil {
		return fmt.Errorf("解析环境变量配置失败：%w", err)
	}
	if len(definitions) == 0 {
		s.SetStep(j, stepName, "skipped", "安装包未声明环境变量")
		return nil
	}
	written, empty := 0, []string{}
	failures := []string{}
	// 环境变量按目标环境逐项幂等写入。
	for _, item := range definitions {
		if reservedEnvKeys[item.Key] {
			continue
		}
		if !item.HasValue {
			empty = append(empty, item.Key)
			continue
		}
		for _, env := range item.Envs {
			var listed map[string]any
			if err := s.Request(ctx, j.Profile, work, "POST", Path(j.TargetID, "/env_vars"), nil,
				map[string]any{"env": env, "scene": 2}, &listed); err != nil {
				failures = append(failures, env+":"+item.Key)
				continue
			}
			body := map[string]any{"key": item.Key, "env": env, "value": item.Value}
			if err := s.Request(ctx, j.Profile, work, "POST",
				Path(j.TargetID, "/create_or_update_env_var"), nil, body, nil); err != nil {
				failures = append(failures, env+":"+item.Key)
				continue
			}
			written++
		}
	}
	if len(failures) > 0 || len(empty) > 0 {
		detail := fmt.Sprintf("已写入 %d 项", written)
		if len(failures) > 0 {
			detail += "；写入失败：" + strings.Join(failures, "、")
		}
		if len(empty) > 0 {
			detail += "；无值需手动补录：" + strings.Join(empty, "、")
		}
		s.SetStep(j, stepName, "warning", detail)
		return nil
	}
	s.SetStep(j, stepName, "success", fmt.Sprintf("已写入 %d 项", written))
	return nil
}

// releaseFullStack 创建发布并等待完成。
func (s *Client) releaseFullStack(ctx context.Context, j *domain.Job, work string) error {
	const stepName = "发布上线"
	s.SetStep(j, stepName, "running", "")
	if j.ReleaseID == "" {
		var created map[string]any
		if err := s.Request(ctx, j.Profile, work, "POST", Path(j.TargetID, "/releases"), nil,
			map[string]any{"branch": Branch}, &created); err != nil {
			return fmt.Errorf("创建发布失败：%w", err)
		}
		j.ReleaseID = nestedString(created, "release_id")
		if j.ReleaseID == "" {
			if release, ok := created["release"].(map[string]any); ok {
				j.ReleaseID = nestedString(release, "release_id")
			}
		}
		if j.ReleaseID == "" {
			j.ReleaseID = nestedString(created, "id")
		}
		if j.ReleaseID == "" {
			return errors.New("妙搭未返回发布 ID")
		}
		if err := s.Jobs.Save(j); err != nil {
			return errors.New("发布已创建，但无法保存发布 ID")
		}
	}

	pollCtx, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	consecutiveFailures := 0
	lastStatus := ""
	for {
		var detail map[string]any
		err := s.Request(pollCtx, j.Profile, work, "GET",
			Path(j.TargetID, "/releases/"+url.PathEscape(j.ReleaseID)), nil, nil, &detail)
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= 10 {
				return fmt.Errorf("发布状态连续查询失败：%w", err)
			}
		} else {
			consecutiveFailures = 0
			status, payload := releasePayload(detail)
			if status != lastStatus {
				log.Printf("[release] target_id=%s release_id=%s status=%q", j.TargetID, j.ReleaseID, status)
				lastStatus = status
			}
			switch status {
			case "finished":
				if address := nestedString(payload, "online_url"); larkcli.HTTPSURL(address) {
					j.OnlineURL = address
				}
				s.SetStep(j, stepName, "success", "release_id="+j.ReleaseID)
				return nil
			case "failed":
				logs := releaseFailureDetails(payload)
				j.ReleaseID = ""
				return errors.New("妙搭发布构建失败：" + strings.Join(logs, "；"))
			}
		}
		select {
		case <-pollCtx.Done():
			return errors.New("发布状态查询超时，可重试查询同一个发布")
		case <-ticker.C:
		}
	}
}

func releasePayload(detail map[string]any) (string, map[string]any) {
	if release, ok := detail["release"].(map[string]any); ok {
		return nestedString(release, "status"), release
	}
	return nestedString(detail, "status"), detail
}

func releaseFailureDetails(detail map[string]any) []string {
	result := []string{}
	if logs, ok := detail["error_logs"].([]any); ok {
		for _, raw := range logs {
			if item, ok := raw.(map[string]any); ok {
				step, message := nestedString(item, "step"), nestedString(item, "error_log")
				if message == "" {
					message = nestedString(item, "message")
				}
				result = append(result, strings.TrimSpace(step+" "+message))
			}
		}
	}
	if len(result) == 0 {
		result = append(result, "请在目标应用发布记录中查看错误")
	}
	return result
}

func nestedString(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	raw, exists := value[key]
	if !exists || raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

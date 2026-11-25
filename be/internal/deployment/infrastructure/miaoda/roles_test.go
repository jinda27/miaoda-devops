package miaoda

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"app-deployment-platform/internal/deployment/domain"
)

func TestDeployerUsesSelectedProfileUser(t *testing.T) {
	client, work := newDDLTestClient(t, `
test "$*" = "auth status --json --profile=target-tenant"
printf '%s' '{"identities":{"user":{"available":true,"openId":"ou_deployer"},"bot":{"available":true,"openId":"ou_bot"}}}'
`)
	// 读取指定租户的用户身份，不调用通讯录或取机器人的身份。
	id, err := client.currentUserOpenID(context.Background(), "target-tenant", work)
	if err != nil || id != "ou_deployer" {
		t.Fatalf("部署者身份错误：id=%q err=%v", id, err)
	}
}

func TestDeployerChecksPermissionBeforeCloudWrites(t *testing.T) {
	client, work := newDDLTestClient(t, `
case "$*" in
  "auth status --json --profile=target-tenant") printf '{}' ;;
  "contact +get-user --as user --json --profile=target-tenant")
    printf '%s' '{"ok":false,"error":{"subtype":"missing_scope","message":"permission required"}}' >&2
    exit 1 ;;
  *) touch unexpected-call; exit 1 ;;
esac
`)
	source := t.TempDir()
	dir := filepath.Join(source, assembleRelativeDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles.md"), []byte("roles:\n- role_id: system_admin\n  name: 管理员\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := client.DeployFullStack(context.Background(), &domain.Job{Profile: "target-tenant"}, work, source)
	// 没有权限时提示具体授权项，且不执行部署写入。
	if err == nil || !strings.Contains(err.Error(), "contact:user.basic_profile:readonly") || !strings.Contains(err.Error(), "同一租户账号") {
		t.Fatalf("缺权限原因不明确：%v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "unexpected-call")); !os.IsNotExist(err) {
		t.Fatalf("身份检查失败后仍执行部署命令：%v", err)
	}
}

func TestDeployerDoesNotUseUnavailableUser(t *testing.T) {
	client, work := newDDLTestClient(t, `
test "$*" = "auth status --json --profile=target-tenant"
printf '%s' '{"identities":{"user":{"available":false,"openId":"ou_expired"},"bot":{"available":true,"openId":"ou_bot"}}}'
`)
	id, err := client.currentUserOpenID(context.Background(), "target-tenant", work)
	// 已失效的用户和机器人都不能用于添加角色成员。
	if err == nil || id != "" || !strings.Contains(err.Error(), "重新授权") {
		t.Fatalf("不可用用户未拦截：id=%q err=%v", id, err)
	}
}

func TestDeployRolesSendsConfirmedUserAsMember(t *testing.T) {
	client, work := newDDLTestClient(t, `
case "$1 $2 $3" in
  "api GET /open-apis/spark/v1/apps/app_test/roles")
    printf '%s' '{"ok":true,"data":{"items":[{"role_id":"system_admin","name":"管理员"}]}}' ;;
  "api POST /open-apis/spark/v1/apps/app_test/roles/system_admin/member_add")
    cat > added-member.json
    printf '%s' '{"ok":true}' ;;
  *) exit 1 ;;
esac
`)
	job := &domain.Job{Profile: "target-tenant", TargetID: "app_test"}
	roles := []roleDefinition{{RoleID: "system_admin", Name: "管理员"}}
	if err := client.deployRoles(context.Background(), job, work, roles, "system_admin", "ou_deployer"); err != nil {
		t.Fatal(err)
	}
	// 角色已存在时仍应提交部署者作为成员。
	body, err := os.ReadFile(filepath.Join(work, "added-member.json"))
	if err != nil || string(body) != `{"users":["ou_deployer"]}` {
		t.Fatalf("成员写入与已确认身份不符：body=%s err=%v", body, err)
	}
}

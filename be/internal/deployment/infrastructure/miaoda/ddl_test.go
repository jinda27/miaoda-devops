package miaoda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"app-deployment-platform/internal/deployment/domain"
	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
)

// 验证分号切分能跳过注释、字符串与 dollar 引用
func TestSplitSQL(t *testing.T) {
	content := `-- 头部注释
CREATE TABLE a (id bigint);
/* 块注释; 不是分隔符 */
CREATE OR REPLACE FUNCTION f() RETURNS int AS $function$
BEGIN
  RETURN 1;
END;
$function$;
INSERT INTO a VALUES ('x;y');
`
	statements := splitSQL(content)
	if len(statements) != 3 {
		t.Fatalf("期望 3 条语句，实际 %d：%#v", len(statements), statements)
	}
	if !strings.Contains(statements[1], "RETURN 1;") {
		t.Fatalf("函数体被错误切分：%s", statements[1])
	}
	if !strings.Contains(statements[2], "'x;y'") {
		t.Fatalf("字符串内分号被错误切分：%s", statements[2])
	}
}

// 验证建表、索引与 INSERT 的幂等改写
func TestMakeDDLStatement(t *testing.T) {
	table := makeDDLStatement(`CREATE TABLE "t1" (id bigint);`).sql
	if !strings.Contains(table, "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("建表未补 IF NOT EXISTS：%s", table)
	}
	index := makeDDLStatement(`CREATE UNIQUE INDEX idx_t1 ON t1 (id);`).sql
	if !strings.Contains(index, "CREATE UNIQUE INDEX IF NOT EXISTS") {
		t.Fatalf("索引未补 IF NOT EXISTS：%s", index)
	}
	sequence := makeDDLStatement(`CREATE SEQUENCE seq_id;`).sql
	if !strings.Contains(sequence, "CREATE SEQUENCE IF NOT EXISTS") {
		t.Fatalf("序列未补 IF NOT EXISTS：%s", sequence)
	}
	insert := makeDDLStatement(`INSERT INTO t1 (id) VALUES (1);`).sql
	if !strings.HasSuffix(strings.TrimSpace(insert), "ON CONFLICT DO NOTHING;") {
		t.Fatalf("INSERT 未补 ON CONFLICT：%s", insert)
	}
	// 已带 IF NOT EXISTS 的语句不得重复追加
	existedIndex := `CREATE INDEX IF NOT EXISTS idx_t1 ON t1 (id);`
	if got := makeDDLStatement(existedIndex).sql; strings.Count(strings.ToUpper(got), "IF NOT EXISTS") != 1 {
		t.Fatalf("索引重复追加 IF NOT EXISTS：%s", got)
	}
	existedTable := `CREATE TABLE IF NOT EXISTS t1 (id bigint);`
	if got := makeDDLStatement(existedTable).sql; strings.Count(strings.ToUpper(got), "IF NOT EXISTS") != 1 {
		t.Fatalf("建表重复追加 IF NOT EXISTS：%s", got)
	}
	// 已带冲突处理的语句保持原样
	kept := `INSERT INTO t1 (id) VALUES (1) ON CONFLICT DO NOTHING;`
	if got := makeDDLStatement(kept).sql; got != kept {
		t.Fatalf("已有 ON CONFLICT 不应改写：%s", got)
	}
	// 无改写规则的语句保留原始注释
	plain := "-- 注释\nCREATE VIEW v1 AS SELECT 1;"
	if got := makeDDLStatement(plain).sql; got != plain {
		t.Fatalf("普通语句不应被改写：%s", got)
	}
}

// 验证建表前自动提取函数与序列依赖。
func TestCollectPreTableDDL(t *testing.T) {
	sequence := `CREATE SEQUENCE ai_calc_snowflake_state;`
	required := `CREATE OR REPLACE FUNCTION workspace_source.ai_calc_next_id()
RETURNS bigint LANGUAGE plpgsql AS $function$
BEGIN
  RETURN nextval('ai_calc_snowflake_state');
END;
$function$;`
	unrelated := `CREATE OR REPLACE FUNCTION workspace_source.after_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$;`
	trigger := `CREATE TRIGGER trg AFTER INSERT ON demo EXECUTE FUNCTION after_insert();`
	units := []ddlUnit{
		{name: "20_tables_001.sql", stmts: []string{
			`CREATE TABLE demo (id bigint DEFAULT ai_calc_next_id ());`,
		}},
		{name: "40_post.sql", stmts: []string{sequence, required, unrelated, trigger}},
	}
	early, selected := collectPreTableDDL(units)
	if len(early) != 2 || early[0] != sequence || early[1] != required {
		t.Fatalf("前置依赖错误：%#v", early)
	}
	if !selected[sequence] || !selected[required] || selected[unrelated] || selected[trigger] {
		t.Fatalf("前置依赖标记错误：%#v", selected)
	}
}

// 验证类型与约束通过存在性检查跳过，而不是 DO 块
func TestMakeDDLStatementGuard(t *testing.T) {
	typeItem := makeDDLStatement(`CREATE TYPE tabledefs AS ENUM ('A', 'B');`)
	if typeItem.guard == nil || typeItem.guard.kind != "type" || typeItem.guard.name != "tabledefs" {
		t.Fatalf("类型未生成守卫：%#v", typeItem.guard)
	}
	constraintItem := makeDDLStatement(`ALTER TABLE "ai_case" ADD CONSTRAINT "ai_case_pkey" PRIMARY KEY (id);`)
	if constraintItem.guard == nil || constraintItem.guard.kind != "constraint" ||
		constraintItem.guard.owner != "ai_case" || constraintItem.guard.name != "ai_case_pkey" {
		t.Fatalf("约束未生成守卫：%#v", constraintItem.guard)
	}
	// 带 schema 前缀的名称需要归一化
	schemaItem := makeDDLStatement(`CREATE TYPE public."tabledefs" AS ENUM ('A');`)
	if schemaItem.guard == nil || schemaItem.guard.name != "tabledefs" {
		t.Fatalf("带 schema 的类型名未归一化：%#v", schemaItem.guard)
	}
}

// 验证旧安装包内重复枚举标签可安全修复。
func TestDeduplicateEnumLabels(t *testing.T) {
	input := `CREATE TYPE tabledefs AS ENUM ('A', 'B', 'PKEY_INTERNAL', 'PKEY_INTERNAL', 'B', 'x,y', 'x,y', 'it''s', 'it''s');`
	item := makeDDLStatement(input)
	if item.repairedEnumLabelCount != 4 {
		t.Fatalf("重复标签计数错误：%d", item.repairedEnumLabelCount)
	}
	if got := item.sql; got != `CREATE TYPE tabledefs AS ENUM ('A', 'B', 'PKEY_INTERNAL', 'x,y', 'it''s');` {
		t.Fatalf("枚举修复错误：%s", got)
	}
	if item.guard == nil || item.guard.name != "tabledefs" {
		t.Fatalf("修复后丢失类型守卫：%#v", item.guard)
	}
}

// 验证正常或无法确认语义的枚举不会被改写。
func TestDeduplicateEnumLabelsKeepsValidSQL(t *testing.T) {
	inputs := []string{
		`CREATE TYPE state AS ENUM ('A', 'B');`,
		`CREATE TYPE state AS ENUM (lower('A'), 'B');`,
		`CREATE TYPE state AS ENUM ('A', 'B);`,
	}
	// 逐条确认非重复定义保持原文。
	for _, input := range inputs {
		got, removed := deduplicateEnumLabels(input)
		if got != input || removed != 0 {
			t.Fatalf("不应改写：%s -> %s (%d)", input, got, removed)
		}
	}
}

// 验证策略与触发器先删后建
func TestMakeDDLStatementRecreate(t *testing.T) {
	policy := makeDDLStatement(`CREATE POLICY "查看" ON "t1" AS PERMISSIVE FOR SELECT USING (true);`).sql
	if !strings.HasPrefix(policy, `DROP POLICY IF EXISTS "查看" ON "t1";`) {
		t.Fatalf("策略未先删除：%s", policy)
	}
	trigger := makeDDLStatement(`CREATE TRIGGER trg_a BEFORE INSERT ON "t1" FOR EACH ROW EXECUTE FUNCTION f();`).sql
	if !strings.HasPrefix(trigger, `DROP TRIGGER IF EXISTS trg_a ON "t1";`) {
		t.Fatalf("触发器未先删除：%s", trigger)
	}
}

// 平台 SQL 接口不支持 DO 块，任何改写都不得生成
func TestNoDoBlock(t *testing.T) {
	inputs := []string{
		`CREATE TYPE t AS ENUM ('A');`,
		`ALTER TABLE t ADD CONSTRAINT c PRIMARY KEY (id);`,
		`CREATE POLICY p ON t USING (true);`,
		`CREATE TABLE t (id bigint);`,
		`CREATE INDEX i ON t (id);`,
		`INSERT INTO t VALUES (1);`,
		`CREATE TRIGGER g BEFORE INSERT ON t EXECUTE FUNCTION f();`,
	}
	for _, input := range inputs {
		got := makeDDLStatement(input).sql
		if strings.Contains(strings.ToUpper(got), "DO $") {
			t.Fatalf("生成了不支持的 DO 块：%s", got)
		}
	}
}

// 验证 workspace 角色与 schema 重写
func TestRewriteWorkspace(t *testing.T) {
	input := `CREATE POLICY p ON t TO "authenticated_workspace_source1" USING (true);`
	got := rewriteWorkspace(input, "target2")
	if !strings.Contains(got, "authenticated_workspace_target2") {
		t.Fatalf("角色未重写：%s", got)
	}
	if strings.Contains(got, "source1") {
		t.Fatalf("仍残留源后缀：%s", got)
	}
	schema := rewriteWorkspace(`CREATE FUNCTION workspace_source1.f() RETURNS int`, "target2")
	if !strings.Contains(schema, "workspace_target2.f()") {
		t.Fatalf("schema 未重写：%s", schema)
	}
}

// 验证从 pg_roles 结果中选取出现最多的后缀
func TestDetectWorkspaceSuffix(t *testing.T) {
	rows := []map[string]any{
		{"rolname": "anon_workspace_abc123"},
		{"rolname": "authenticated_workspace_abc123"},
		{"rolname": "service_role_workspace_abc123"},
		{"rolname": "user_authenticated_workspace_abc123"},
		{"rolname": "anon_workspace_zzz999"},
	}
	if got := detectWorkspaceSuffix(rows); got != "abc123" {
		t.Fatalf("期望 abc123，实际 %s", got)
	}
}

// 验证 SQL 接口二次 JSON 编码结果的展开
func TestParseSQLResult(t *testing.T) {
	inner := `[{"rolname":"anon_workspace_abc"}]`
	encoded, _ := json.Marshal([]string{inner})
	rows := parseSQLResult(encoded)
	if len(rows) != 1 || rows[0]["rolname"] != "anon_workspace_abc" {
		t.Fatalf("解析结果异常：%#v", rows)
	}
	if got := parseSQLResult(json.RawMessage(`""`)); len(got) != 0 {
		t.Fatalf("空结果应返回空集合：%#v", got)
	}
}

// 验证存在性集合的键构造
func TestDDLGuardKeys(t *testing.T) {
	rows := []map[string]any{
		{"kind": "type", "owner_name": "", "object_name": "tabledefs"},
		{"kind": "constraint", "owner_name": "ai_case", "object_name": "ai_case_pkey"},
	}
	existing := ddlGuardKeys(rows)
	if !existing[ddlGuard{kind: "type", name: "tabledefs"}.key()] {
		t.Fatal("类型守卫键缺失")
	}
	if !existing[ddlGuard{kind: "constraint", owner: "ai_case", name: "ai_case_pkey"}.key()] {
		t.Fatal("约束守卫键缺失")
	}
}

// 验证平台默认策略与扩展对象被过滤
func TestDDLStatementFilters(t *testing.T) {
	if !isPlatformPolicy(`CREATE POLICY "service_role_bypass_policy" ON t USING (true);`) {
		t.Fatal("平台默认策略未被识别")
	}
	if isPlatformPolicy(`CREATE POLICY "业务策略" ON t USING (true);`) {
		t.Fatal("业务策略被误过滤")
	}
	if !isExtensionObject(`CREATE OR REPLACE VIEW pg_stat_statements AS SELECT 1;`) {
		t.Fatal("扩展视图未被识别")
	}
}

// 验证阶段顺序校验
func TestDDLPhaseOrder(t *testing.T) {
	if ddlPhaseOrder(ddlPhase("10_types.sql")) >= ddlPhaseOrder(ddlPhase("20_tables_001.sql")) {
		t.Fatal("types 必须早于 tables")
	}
	if ddlPhaseOrder(ddlPhase("40_post.sql")) >= ddlPhaseOrder(ddlPhase("90_inserts_roles.sql")) {
		t.Fatal("post 必须早于 inserts")
	}
}

// 验证实际启动三个协程且全部文件恰好执行一次。
func TestRunDDLStageConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	units := make([]ddlUnit, 9)
	// 生成超过协程上限的任务。
	for i := range units {
		units[i].name = fmt.Sprint(i)
	}
	started := make(chan string, len(units))
	release := make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	var active, maximum atomic.Int32
	go func() {
		done <- runDDLStage(ctx, units, func(ctx context.Context, unit ddlUnit) error {
			current := active.Add(1)
			defer active.Add(-1)
			// 用原子更新记录同时执行的最大数量。
			for previous := maximum.Load(); current > previous; previous = maximum.Load() {
				if maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			started <- unit.name
			// 阻塞首批任务，验证队列不能启动第四个文件。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		})
	}()
	seen := map[string]bool{}
	// 首批必须同时启动三个文件。
	for i := 0; i < 3; i++ {
		select {
		case name := <-started:
			seen[name] = true
		case <-ctx.Done():
			t.Fatal("未同时启动三个文件")
		}
	}
	// 前三个文件未释放时不得执行第四个。
	select {
	case name := <-started:
		t.Fatalf("超过并发上限：%s", name)
	default:
	}
	// 逐个释放全部任务，允许空闲协程领取后续文件。
	for range units {
		select {
		case release <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("协程池未完成任务")
		}
	}
	// 等待协程退出后核对执行结果。
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("协程池未退出")
	}
	close(started)
	// 每个文件只允许出现一次。
	for name := range started {
		if seen[name] {
			t.Fatalf("重复执行文件：%s", name)
		}
		seen[name] = true
	}
	if maximum.Load() != 3 || len(seen) != len(units) || active.Load() != 0 {
		t.Fatalf("并发统计错误：maximum=%d files=%d active=%d", maximum.Load(), len(seen), active.Load())
	}
}

// 验证首次失败取消在途文件并停止领取剩余文件。
func TestRunDDLStageFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failure := errors.New("测试 DDL 失败")
	firstBatch := make(chan struct{})
	var started, exited atomic.Int32
	units := []ddlUnit{{name: "fail"}, {name: "wait1"}, {name: "wait2"}, {name: "pending"}}
	err := runDDLStage(ctx, units, func(ctx context.Context, unit ddlUnit) error {
		defer exited.Add(1)
		// 三个请求均启动后触发失败。
		if started.Add(1) == 3 {
			close(firstBatch)
		}
		select {
		case <-firstBatch:
		case <-ctx.Done():
			return ctx.Err()
		}
		// 一个文件失败，其余文件等待取消。
		if unit.name == "fail" {
			return failure
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, failure) || started.Load() != 3 || exited.Load() != 3 {
		t.Fatalf("失败未正确停止：err=%v started=%d exited=%d", err, started.Load(), exited.Load())
	}
	// 已取消的阶段不应执行任何文件。
	cancel()
	if err := runDDLStage(ctx, units, func(context.Context, ddlUnit) error {
		t.Error("取消后仍执行文件")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误未保留：%v", err)
	}
}

// newDDLTestClient 使用本地 CLI 替身，禁止真实调用平台。
func newDDLTestClient(t *testing.T, script string) (*Client, string) {
	t.Helper()
	work := t.TempDir()
	binary := filepath.Join(work, "lark-cli")
	// 写入可执行的请求替身。
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAODA_LARK_CLI", binary)
	return &Client{Lark: larkcli.NewClient(work)}, work
}

// 验证妙搭和 SQL 透传入口隔离外部凭证。
func TestRequestsIgnoreExternalCredentials(t *testing.T) {
	client, work := newDDLTestClient(t, `
# 两个透传入口都不能使用工具注入的凭证。
if printenv LARKSUITE_CLI_USER_ACCESS_TOKEN >/dev/null 2>&1 ||
   printenv LARKSUITE_CLI_APP_ID >/dev/null 2>&1; then exit 1; fi
printf '{"data":{"result":[]}}'
`)
	t.Setenv("LARKSUITE_CLI_USER_ACCESS_TOKEN", "external-test-token")
	t.Setenv("LARKSUITE_CLI_APP_ID", "external-test-app")
	ctx := context.Background()
	if err := client.Request(ctx, "tenant", work, "GET", "/test", nil, nil, nil); err != nil {
		t.Fatalf("妙搭接口未隔离外部凭证：%v", err)
	}
	if _, err := client.sparkSQL(ctx, "tenant", "app_test", "dev", "SELECT 1", work); err != nil {
		t.Fatalf("SQL 接口未隔离外部凭证：%v", err)
	}
}

// 验证并发请求独立读写响应文件且完成后清理文件。
func TestSparkSQLConcurrentResponses(t *testing.T) {
	client, work := newDDLTestClient(t, `
while [ "$#" -gt 0 ]; do
  case "$1" in
    --data) data="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "$output" >> output-paths
printf '{"data":{"result":[[%s]]}}' "$data" > "$output"
touch "$output.ready"
while [ "$(ls .sql-result-*.json.ready | wc -l)" -lt 3 ]; do
  sleep 0.01
done
printf '{"data":{"result":[]}}'
`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	units := []ddlUnit{{name: "one"}, {name: "two"}, {name: "three"}}
	err := runDDLStage(ctx, units, func(ctx context.Context, unit ddlUnit) error {
		rows, err := client.sparkSQL(ctx, "test", "app_test", "dev", unit.name, work)
		if err != nil {
			return err
		}
		// 每个请求必须读取属于自己的文件响应。
		if len(rows) != 1 || rows[0]["sql"] != unit.name {
			return fmt.Errorf("请求 %s 读取了错误响应：%v", unit.name, rows)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := os.ReadFile(filepath.Join(work, "output-paths"))
	if err != nil {
		t.Fatal(err)
	}
	unique := map[string]bool{}
	// 检查响应路径不重复且请求完成后均已删除。
	for _, path := range strings.Fields(string(paths)) {
		if unique[path] {
			t.Fatalf("并发请求共用响应文件：%s", path)
		}
		unique[path] = true
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("响应文件未清理：%s err=%v", path, err)
		}
	}
	if len(unique) != 3 {
		t.Fatalf("预期三个响应文件，实际 %d", len(unique))
	}
}

// 验证阶段屏障、建表前置依赖和失败后的后续阶段阻断。
func TestRunDDLStageOrder(t *testing.T) {
	// 分别验证正常执行和建表失败。
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			client, work := newDDLTestClient(t, `
while [ "$#" -gt 0 ]; do
  case "$1" in
    --data) data="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    *) shift ;;
  esac
done
case "$data" in
  *"CREATE TYPE"*) touch types.ready ;;
  *"CREATE SEQUENCE"*) test -f types.ready; touch dependencies.ready ;;
  *"CREATE TABLE"*)
    test -f types.ready
    test -f dependencies.ready
    case "$data" in
      *"demo ("*)
        if [ "$TEST_DDL_FAIL" = 1 ]; then echo "建表失败" >&2; exit 1; fi
        touch demo.ready ;;
      *"other ("*) touch other.ready ;;
      *"third ("*) touch third.ready ;;
      *"fourth ("*) touch fourth.ready ;;
    esac ;;
  *"FOREIGN KEY"*)
    test -f demo.ready
    test -f other.ready
    test -f third.ready
    test -f fourth.ready
    touch foreign-keys.ready ;;
  *"after_insert"*) test -f foreign-keys.ready; touch post.ready ;;
  *"INSERT INTO"*) test -f post.ready; touch inserts.ready ;;
esac
printf '{"data":{"result":[]}}' > "$output"
`)
			t.Setenv("TEST_DDL_FAIL", "0")
			// 失败场景只让指定建表文件返回错误。
			if fail {
				t.Setenv("TEST_DDL_FAIL", "1")
			}
			source := t.TempDir()
			dir := filepath.Join(source, filepath.FromSlash(ddlRelativeDir))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				"10_types.sql":        `CREATE TYPE state AS ENUM ('A');`,
				"20_tables_001.sql":   `CREATE TABLE demo (id bigint DEFAULT next_id());`,
				"20_tables_002.sql":   `CREATE TABLE other (id bigint);`,
				"20_tables_003.sql":   `CREATE TABLE third (id bigint);`,
				"20_tables_004.sql":   `CREATE TABLE fourth (id bigint);`,
				"30_foreign_keys.sql": `ALTER TABLE other ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES demo (id);`,
				"40_post.sql": `CREATE SEQUENCE demo_seq;
CREATE FUNCTION next_id() RETURNS bigint LANGUAGE sql AS $$ SELECT nextval('demo_seq'); $$;
CREATE FUNCTION after_insert() RETURNS int LANGUAGE sql AS $$ SELECT 1; $$;`,
				"90_inserts.sql": `INSERT INTO demo VALUES (1);`,
			}
			// 用真实安装包文件路径驱动完整调度。
			for name, sql := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := client.runDDL(ctx, &domain.Job{ID: "test", TargetID: "app_test"}, source, work)
			// 成功与失败场景都不能遗留请求响应文件。
			if paths, scanErr := filepath.Glob(filepath.Join(work, ".sql-result-*.json")); scanErr != nil || len(paths) != 0 {
				t.Fatalf("响应文件未清理：paths=%v err=%v", paths, scanErr)
			}
			// 失败必须包含文件名，且后置对象及数据阶段未执行。
			if fail {
				if err == nil || !strings.Contains(err.Error(), "20_tables_001.sql") {
					t.Fatalf("未报告建表文件失败：%v", err)
				}
				for _, name := range []string{"foreign-keys.ready", "post.ready", "inserts.ready"} {
					if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
						t.Fatalf("失败后仍执行后续阶段：%s", name)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// 数据完成标记证明全部阶段按依赖顺序执行。
			if _, err := os.Stat(filepath.Join(work, "inserts.ready")); err != nil {
				t.Fatalf("初始化数据未执行：%v", err)
			}
		})
	}
}

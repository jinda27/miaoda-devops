package miaoda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"app-deployment-platform/internal/deployment/domain"
	"app-deployment-platform/internal/deployment/infrastructure/larkcli"
)

// DDL 按阶段执行，同阶段最多三个文件并发提交。

const (
	ddlRelativeDir = "docs/agent_assemble/ddl"
	ddlPhaseTypes  = "types"
	ddlPhaseTables = "tables"
	ddlPhasePost   = "post"
	ddlPhaseInsert = "inserts"
	ddlConcurrency = 3
)

var (
	// 拆分 DDL 规范文件名，前缀数字决定执行顺序
	ddlFilePattern = regexp.MustCompile(`^\d{2}_[a-z0-9_]+\.sql$`)
	// 源应用 workspace 角色名，形如 authenticated_workspace_xxx
	wsRolePattern = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*_workspace)_([a-z0-9]+)\b`)
	// 源应用 workspace schema 名
	wsSchemaPattern = regexp.MustCompile(`\bworkspace_([a-z0-9]+)\b`)
	// 建表语句，用于补 IF NOT EXISTS
	createTablePrefix = "CREATE TABLE "
	// 序列语句，用于补 IF NOT EXISTS
	createSequencePrefix = "CREATE SEQUENCE "
	// 索引语句前缀，保留 UNIQUE 与 CONCURRENTLY 修饰
	indexPrefixPattern = regexp.MustCompile(`(?i)^(CREATE\s+(?:UNIQUE\s+)?INDEX(?:\s+CONCURRENTLY)?)\s+`)
	// 策略名与所属表，用于先删后建
	policyTargetPattern = regexp.MustCompile(`(?is)^CREATE\s+POLICY\s+(\S+)\s+ON\s+(\S+)`)
	// 触发器名与所属表，用于先删后建
	triggerTargetPattern = regexp.MustCompile(`(?is)^CREATE\s+TRIGGER\s+(\S+)\s+.*?\sON\s+(\S+)`)
	// 类型名，用于执行前检查是否已存在
	typeNamePattern = regexp.MustCompile(`(?i)^CREATE\s+TYPE\s+([^\s(]+)`)
	// 枚举定义，用于兼容旧安装包中的重复标签
	enumTypePattern = regexp.MustCompile(`(?is)^CREATE\s+TYPE\s+[^\s(]+\s+AS\s+ENUM\s*\((.*)\)\s*;?\s*$`)
	// 函数名，用于识别建表前置依赖
	functionNamePattern = regexp.MustCompile(`(?i)^CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+([^\s(]+)\s*\(`)
	// 函数调用，用于匹配列默认值等表达式
	functionCallPattern = regexp.MustCompile(`(?i)((?:"[^"]+"|[A-Za-z_][A-Za-z0-9_]*)(?:\s*\.\s*(?:"[^"]+"|[A-Za-z_][A-Za-z0-9_]*))?)\s*\(`)
	// 序列名，用于补齐前置函数依赖
	sequenceNamePattern = regexp.MustCompile(`(?i)^CREATE\s+SEQUENCE\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s;]+)`)
	// 表名与约束名，用于执行前检查是否已存在
	constraintPattern = regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([^\s]+)\s+.*?\bADD\s+CONSTRAINT\s+([^\s]+)`)
	insertHeadPattern = regexp.MustCompile(`(?i)^INSERT\s+INTO\b`)
	onConflictPattern = regexp.MustCompile(`(?i)\bON\s+CONFLICT\b`)
	returningPattern  = regexp.MustCompile(`(?i)\bRETURNING\b`)
)

// 平台随表自动生成的默认策略，不需要重复创建
var platformPolicies = map[string]bool{
	"service_role_bypass_policy": true,
	"查看全部数据":                     true,
	"修改全部数据":                     true,
	"修改本人数据":                     true,
}

// ddlUnit 表示一个待执行的 SQL 文件
type ddlUnit struct {
	name  string
	stmts []string
}

// ddlGuard 描述执行前需要确认是否已存在的对象
type ddlGuard struct {
	kind  string
	owner string
	name  string
}

// key 生成对象唯一标识
func (g ddlGuard) key() string {
	return g.kind + "|" + g.owner + "|" + g.name
}

// ddlStatement 是一条可执行语句；guard 非空时需先确认对象不存在
type ddlStatement struct {
	sql                    string
	guard                  *ddlGuard
	repairedEnumLabelCount int
}

// runDDL 按阶段读取 DDL，阶段内并发提交文件。
func (s *Client) runDDL(ctx context.Context, j *domain.Job, source, work string) error {
	started := time.Now()
	dir := filepath.Join(source, filepath.FromSlash(ddlRelativeDir))
	names, err := ddlFileNames(dir)
	if err != nil {
		log.Printf("[ddl] load_failed job_id=%s target_id=%s error=%q", j.ID, j.TargetID, err)
		return err
	}
	units, err := loadDDLUnits(dir, names)
	if err != nil {
		log.Printf("[ddl] parse_failed job_id=%s target_id=%s error=%q", j.ID, j.TargetID, err)
		return err
	}
	log.Printf("[ddl] started job_id=%s target_id=%s files=%d concurrency=%d",
		j.ID, j.TargetID, len(units), ddlConcurrency)
	earlyStatements, earlySet := collectPreTableDDL(units)
	// 探测目标库 workspace 后缀，用于改写角色与 schema 名
	suffix := ""
	rows, queryErr := s.sparkSQL(ctx, j.Profile, j.TargetID, "dev", "SELECT rolname FROM pg_roles;", work)
	if queryErr == nil {
		suffix = detectWorkspaceSuffix(rows)
		log.Printf("[ddl] workspace_detected job_id=%s target_id=%s suffix=%q", j.ID, j.TargetID, suffix)
	} else {
		log.Printf("[ddl] workspace_detect_failed job_id=%s target_id=%s error=%q", j.ID, j.TargetID, queryErr)
	}
	// 等待当前阶段全部完成后才进入下一阶段。
	for start := 0; start < len(units); {
		phase := ddlPhase(units[start].name)
		end := start + 1
		// 同阶段连续文件交给固定大小的协程池。
		for end < len(units) && ddlPhase(units[end].name) == phase {
			end++
		}
		// 取消后不再执行前置依赖或启动新阶段。
		if err := ctx.Err(); err != nil {
			return err
		}
		// 建表阶段前串行创建默认值依赖的函数与序列。
		if phase == ddlPhaseTables {
			prepared := make([]string, 0, len(earlyStatements))
			// 保留前置依赖在安装包中的顺序。
			for _, stmt := range earlyStatements {
				prepared = append(prepared, makeDDLStatement(rewriteWorkspace(stmt, suffix)).sql)
			}
			// 仅在存在前置依赖时提交。
			if len(prepared) > 0 {
				earlyStarted := time.Now()
				log.Printf("[ddl] pre_table_started job_id=%s statements=%d", j.ID, len(prepared))
				if _, err := s.sparkSQL(ctx, j.Profile, j.TargetID, "dev", strings.Join(prepared, "\n"), work); err != nil {
					return fmt.Errorf("建表前置依赖执行失败。%w", err)
				}
				log.Printf("[ddl] pre_table_finished job_id=%s status=success duration_ms=%d",
					j.ID, time.Since(earlyStarted).Milliseconds())
			}
		}
		stageStarted := time.Now()
		log.Printf("[ddl] phase_started job_id=%s phase=%s files=%d", j.ID, phase, end-start)
		// 任一文件失败时取消同阶段请求，禁止进入后续阶段。
		if err := runDDLStage(ctx, units[start:end], func(ctx context.Context, unit ddlUnit) error {
			return s.runDDLUnit(ctx, j, unit, suffix, earlySet, work)
		}); err != nil {
			return err
		}
		log.Printf("[ddl] phase_finished job_id=%s phase=%s status=success duration_ms=%d",
			j.ID, phase, time.Since(stageStarted).Milliseconds())
		start = end
	}
	log.Printf("[ddl] finished job_id=%s target_id=%s status=success duration_ms=%d",
		j.ID, j.TargetID, time.Since(started).Milliseconds())
	return nil
}

// runDDLStage 限制阶段内并发数，失败后取消并等待全部协程退出。
func runDDLStage(ctx context.Context, units []ddlUnit, execute func(context.Context, ddlUnit) error) error {
	stageCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan ddlUnit)
	var workers sync.WaitGroup
	var firstFailure sync.Once
	var firstErr error
	// 最多启动三个工作协程，不为每个文件创建协程。
	for i := 0; i < ddlConcurrency && i < len(units); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			// 逐个领取文件，取消后不再执行新文件。
			for unit := range queue {
				if stageCtx.Err() != nil {
					return
				}
				// 保留首次业务错误，取消其他在途请求。
				if err := execute(stageCtx, unit); err != nil {
					firstFailure.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
dispatch:
	// 无缓冲队列保证空闲协程才领取下一个文件。
	for _, unit := range units {
		select {
		case <-stageCtx.Done():
			break dispatch
		case queue <- unit:
		}
	}
	close(queue)
	workers.Wait()
	// 首次文件错误优先于其引起的取消错误。
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// runDDLUnit 保留单文件语句顺序、幂等改写和存在性检查。
func (s *Client) runDDLUnit(
	ctx context.Context, j *domain.Job, unit ddlUnit, suffix string, earlySet map[string]bool, work string,
) error {
	// 空文件直接跳过。
	if len(unit.stmts) == 0 {
		log.Printf("[ddl] file_skipped job_id=%s file=%q reason=empty", j.ID, unit.name)
		return nil
	}
	prepared := make([]ddlStatement, 0, len(unit.stmts))
	needGuard := false
	// 按文件顺序改写每条 SQL。
	for _, stmt := range unit.stmts {
		// 前置依赖已执行，后置阶段不重复提交。
		if earlySet[stmt] {
			continue
		}
		item := makeDDLStatement(rewriteWorkspace(stmt, suffix))
		// 记录旧安装包中的枚举修复。
		if item.repairedEnumLabelCount > 0 {
			log.Printf("[ddl] enum_repaired job_id=%s file=%q removed_duplicate_labels=%d",
				j.ID, unit.name, item.repairedEnumLabelCount)
		}
		// 类型与约束需要存在性检查。
		if item.guard != nil {
			needGuard = true
		}
		prepared = append(prepared, item)
	}
	existing := map[string]bool{}
	// 接口不支持 DO 块，先查对象状态再决定是否执行。
	if needGuard {
		rows, err := s.sparkSQL(ctx, j.Profile, j.TargetID, "dev", ddlGuardQuery(), work)
		if err != nil {
			return fmt.Errorf("无法确认 DDL 对象状态（%s）。%w", unit.name, err)
		}
		existing = ddlGuardKeys(rows)
	}
	statements := make([]string, 0, len(prepared))
	skipped := 0
	// 已存在的类型或约束直接跳过。
	for _, item := range prepared {
		if item.guard != nil && existing[item.guard.key()] {
			skipped++
			continue
		}
		statements = append(statements, item.sql)
	}
	// 过滤后无待执行语句时跳过请求。
	if len(statements) == 0 {
		log.Printf("[ddl] file_skipped job_id=%s file=%q total=%d existing=%d",
			j.ID, unit.name, len(prepared), skipped)
		return nil
	}
	fileStarted := time.Now()
	log.Printf("[ddl] file_started job_id=%s file=%q statements=%d existing_skipped=%d",
		j.ID, unit.name, len(statements), skipped)
	// 一个文件合并为一次请求，等待请求完成。
	if _, err := s.sparkSQL(ctx, j.Profile, j.TargetID, "dev", strings.Join(statements, "\n"), work); err != nil {
		log.Printf("[ddl] file_finished job_id=%s file=%q status=failed duration_ms=%d error=%q",
			j.ID, unit.name, time.Since(fileStarted).Milliseconds(), err)
		return fmt.Errorf("DDL 执行失败（%s）。%w", unit.name, err)
	}
	log.Printf("[ddl] file_finished job_id=%s file=%q status=success duration_ms=%d",
		j.ID, unit.name, time.Since(fileStarted).Milliseconds())
	return nil
}

// collectPreTableDDL 收集建表表达式依赖的函数及其序列。
func collectPreTableDDL(units []ddlUnit) ([]string, map[string]bool) {
	tableCalls := map[string]bool{}
	functions := map[string]string{}
	sequences := map[string]string{}
	postOrder := []string{}
	// 扫描建表调用与后置对象定义。
	for _, unit := range units {
		for _, stmt := range unit.stmts {
			core := strings.TrimSpace(stripLeadingComments(stmt))
			switch ddlPhase(unit.name) {
			case ddlPhaseTables:
				for _, match := range functionCallPattern.FindAllStringSubmatch(core, -1) {
					tableCalls[normalizeIdentifier(match[1])] = true
				}
			case ddlPhasePost:
				postOrder = append(postOrder, stmt)
				if match := functionNamePattern.FindStringSubmatch(core); match != nil {
					functions[normalizeIdentifier(match[1])] = stmt
				}
				if match := sequenceNamePattern.FindStringSubmatch(core); match != nil {
					sequences[normalizeIdentifier(match[1])] = stmt
				}
			}
		}
	}
	selectedFunctions := map[string]bool{}
	functionSQL := strings.Builder{}
	// 仅提前执行建表阶段实际调用的函数。
	for name := range tableCalls {
		if stmt := functions[name]; stmt != "" {
			selectedFunctions[stmt] = true
			functionSQL.WriteString("\n")
			functionSQL.WriteString(stmt)
		}
	}
	selectedSequences := map[string]bool{}
	// 补齐前置函数引用的序列。
	for name, stmt := range sequences {
		if containsSQLIdentifier(functionSQL.String(), name) {
			selectedSequences[stmt] = true
		}
	}
	early := []string{}
	selected := map[string]bool{}
	// 按 40_post.sql 原顺序输出依赖。
	for _, stmt := range postOrder {
		if selectedFunctions[stmt] || selectedSequences[stmt] {
			early = append(early, stmt)
			selected[stmt] = true
		}
	}
	return early, selected
}

// normalizeIdentifier 归一化 schema 与引号格式。
func normalizeIdentifier(raw string) string {
	return strings.ToLower(normalizeName(strings.Join(strings.Fields(raw), "")))
}

// containsSQLIdentifier 判断 SQL 是否引用指定对象。
func containsSQLIdentifier(sql, name string) bool {
	pattern := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `([^A-Za-z0-9_]|$)`)
	return pattern.MatchString(sql)
}

// ddlGuardQuery 查询当前 schema 下已存在的类型与约束
func ddlGuardQuery() string {
	return `SELECT 'type' AS kind, '' AS owner_name, typname AS object_name
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = current_schema()
UNION ALL
SELECT 'constraint', c.relname, con.conname
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema();`
}

// ddlGuardKeys 把查询结果转换为存在性集合
func ddlGuardKeys(rows []map[string]any) map[string]bool {
	existing := map[string]bool{}
	for _, row := range rows {
		guard := ddlGuard{
			kind:  fmt.Sprint(row["kind"]),
			owner: normalizeName(fmt.Sprint(row["owner_name"])),
			name:  normalizeName(fmt.Sprint(row["object_name"])),
		}
		existing[guard.key()] = true
	}
	return existing
}

// normalizeName 去掉引号与 schema 前缀
func normalizeName(raw string) string {
	name := strings.Trim(strings.TrimSpace(raw), `"`)
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	return strings.Trim(name, `"`)
}

// ddlFileNames 返回目录内符合规范的 SQL 文件名，按文件名升序。
func ddlFileNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("安装包缺少 docs/agent_assemble/ddl 目录，无法执行 DDL")
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !ddlFilePattern.MatchString(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, errors.New("docs/agent_assemble/ddl 下没有可执行的 SQL 文件")
	}
	sort.Strings(names)
	return names, nil
}

// loadDDLUnits 读取并过滤每个文件的语句，同时校验阶段顺序不倒退。
func loadDDLUnits(dir string, names []string) ([]ddlUnit, error) {
	units := make([]ddlUnit, 0, len(names))
	lastPhase := 0
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("无法读取 DDL 文件 %s", name)
		}
		phase := ddlPhase(name)
		if order := ddlPhaseOrder(phase); order < lastPhase {
			return nil, fmt.Errorf("DDL 阶段顺序错误：%s 不能出现在更晚阶段之后", name)
		} else {
			lastPhase = order
		}
		statements := []string{}
		for _, stmt := range splitSQL(string(content)) {
			// 平台默认策略与扩展自动对象由平台负责，跳过
			if isPlatformPolicy(stmt) || isExtensionObject(stmt) {
				continue
			}
			statements = append(statements, stmt)
		}
		units = append(units, ddlUnit{name: name, stmts: statements})
	}
	return units, nil
}

// ddlPhase 根据文件名推断阶段。
func ddlPhase(name string) string {
	stem := strings.TrimSuffix(strings.ToLower(name), ".sql")
	switch {
	case strings.Contains(stem, "extension"):
		return "extensions"
	case strings.Contains(stem, "types"):
		return ddlPhaseTypes
	case strings.Contains(stem, "foreign_keys"):
		return "foreign_keys"
	case strings.Contains(stem, "tables"):
		return ddlPhaseTables
	case strings.Contains(stem, "insert"):
		return ddlPhaseInsert
	}
	return ddlPhasePost
}

// ddlPhaseOrder 返回阶段序号，用于顺序校验。
func ddlPhaseOrder(phase string) int {
	switch phase {
	case "extensions":
		return 5
	case ddlPhaseTypes:
		return 10
	case ddlPhaseTables:
		return 20
	case "foreign_keys":
		return 30
	case ddlPhasePost:
		return 40
	case ddlPhaseInsert:
		return 90
	}
	return 40
}

// isPlatformPolicy 判断是否为平台自动生成的默认策略。
func isPlatformPolicy(statement string) bool {
	core := stripLeadingComments(statement)
	if !hasPrefixFold(core, "CREATE POLICY ") {
		return false
	}
	rest := strings.TrimSpace(core[len("CREATE POLICY "):])
	if rest == "" {
		return false
	}
	name := rest
	if idx := strings.IndexAny(rest, " \t\r\n"); idx >= 0 {
		name = rest[:idx]
	}
	name = strings.Trim(name, `"`)
	return platformPolicies[name]
}

// isExtensionObject 过滤扩展自动生成的对象，扩展由 05_extensions.sql 创建。
func isExtensionObject(statement string) bool {
	core := stripLeadingComments(statement)
	if strings.Contains(strings.ToLower(core), "language c") && strings.Contains(core, "$libdir") {
		return true
	}
	if !hasPrefixFold(core, "CREATE OR REPLACE VIEW ") {
		return false
	}
	rest := strings.TrimSpace(core[len("CREATE OR REPLACE VIEW "):])
	rest = strings.Trim(rest, `"`)
	if idx := strings.IndexAny(rest, " \t\r\n("); idx >= 0 {
		rest = rest[:idx]
	}
	return rest == "pg_stat_statements" || rest == "pg_stat_statements_info"
}

// rewriteWorkspace 把源应用的 workspace 角色与 schema 改写为目标应用。
func rewriteWorkspace(statement, suffix string) string {
	if suffix == "" {
		return statement
	}
	rewritten := wsRolePattern.ReplaceAllString(statement, "${1}_"+suffix)
	return wsSchemaPattern.ReplaceAllString(rewritten, "workspace_"+suffix)
}

// detectWorkspaceSuffix 从 pg_roles 结果中取出现次数最多的 workspace 后缀。
func detectWorkspaceSuffix(rows []map[string]any) string {
	counts := map[string]int{}
	for _, row := range rows {
		name := fmt.Sprint(row["rolname"])
		for _, match := range wsRolePattern.FindAllStringSubmatch(name, -1) {
			counts[match[2]]++
		}
	}
	best, bestCount := "", 0
	for suffix, count := range counts {
		if count > bestCount {
			best, bestCount = suffix, count
		}
	}
	return best
}

// makeDDLIdempotent 把常见 DDL 改写为可安全重跑的形式。
// makeDDLStatement 改写单条语句，并返回存在性守卫。
// 平台 SQL 接口不支持 DO 块，不能靠 plpgsql 捕获重复对象错误。
func makeDDLStatement(statement string) ddlStatement {
	core := strings.TrimSpace(stripLeadingComments(statement))
	if core == "" {
		return ddlStatement{sql: statement}
	}
	// 类型不支持 IF NOT EXISTS，改为执行前检查
	if match := typeNamePattern.FindStringSubmatch(core); match != nil {
		repaired, removed := deduplicateEnumLabels(core)
		return ddlStatement{
			sql: repaired, guard: &ddlGuard{kind: "type", name: normalizeName(match[1])},
			repairedEnumLabelCount: removed,
		}
	}
	// 新增约束同样不支持 IF NOT EXISTS
	if match := constraintPattern.FindStringSubmatch(core); match != nil {
		return ddlStatement{sql: core, guard: &ddlGuard{
			kind: "constraint", owner: normalizeName(match[1]), name: normalizeName(match[2]),
		}}
	}
	// 策略先删后建，避免重复报错
	if hasPrefixFold(core, "CREATE POLICY ") {
		if match := policyTargetPattern.FindStringSubmatch(core); match != nil {
			return ddlStatement{sql: "DROP POLICY IF EXISTS " + match[1] + " ON " + match[2] + ";\n" + core}
		}
		return ddlStatement{sql: core}
	}
	// 触发器不支持 OR REPLACE，先删后建
	if hasPrefixFold(core, "CREATE TRIGGER ") {
		if match := triggerTargetPattern.FindStringSubmatch(core); match != nil {
			return ddlStatement{sql: "DROP TRIGGER IF EXISTS " + match[1] + " ON " + match[2] + ";\n" + core}
		}
		return ddlStatement{sql: core}
	}
	// 建表补 IF NOT EXISTS
	if hasPrefixFold(core, createTablePrefix) {
		rest := strings.TrimSpace(core[len(createTablePrefix):])
		if !hasPrefixFold(rest, "IF NOT EXISTS") {
			return ddlStatement{sql: createTablePrefix + "IF NOT EXISTS " + rest}
		}
		return ddlStatement{sql: core}
	}
	// 序列补 IF NOT EXISTS
	if hasPrefixFold(core, createSequencePrefix) {
		rest := strings.TrimSpace(core[len(createSequencePrefix):])
		if !hasPrefixFold(rest, "IF NOT EXISTS") {
			return ddlStatement{sql: createSequencePrefix + "IF NOT EXISTS " + rest}
		}
		return ddlStatement{sql: core}
	}
	// 索引补 IF NOT EXISTS
	if loc := indexPrefixPattern.FindStringSubmatchIndex(core); loc != nil {
		prefix := strings.TrimSpace(core[:loc[3]])
		rest := strings.TrimSpace(core[loc[3]:])
		if !hasPrefixFold(rest, "IF NOT EXISTS") {
			return ddlStatement{sql: prefix + " IF NOT EXISTS " + rest}
		}
		return ddlStatement{sql: core}
	}
	// INSERT 补冲突处理，带 RETURNING 的保持原样
	if insertHeadPattern.MatchString(core) && !onConflictPattern.MatchString(core) && !returningPattern.MatchString(core) {
		return ddlStatement{sql: strings.TrimRight(core, ";") + " ON CONFLICT DO NOTHING;"}
	}
	// 未命中改写规则时保留原始语句（含注释）
	return ddlStatement{sql: statement}
}

// deduplicateEnumLabels 去除旧安装包中重复的枚举标签。
func deduplicateEnumLabels(statement string) (string, int) {
	match := enumTypePattern.FindStringSubmatchIndex(statement)
	if match == nil {
		return statement, 0
	}
	labels, valid := splitEnumLabels(statement[match[2]:match[3]])
	if !valid {
		return statement, 0
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(labels))
	removed := 0
	// 保留每个标签的首次定义。
	for _, label := range labels {
		key := strings.ReplaceAll(label[1:len(label)-1], "''", "'")
		if seen[key] {
			removed++
			continue
		}
		seen[key] = true
		unique = append(unique, label)
	}
	if removed == 0 {
		return statement, 0
	}
	return statement[:match[2]] + strings.Join(unique, ", ") + statement[match[3]:], removed
}

// splitEnumLabels 按 SQL 字符串边界拆分枚举标签。
func splitEnumLabels(body string) ([]string, bool) {
	labels := []string{}
	start := 0
	quoted := false
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '\'' && quoted && i+1 < len(body) && body[i+1] == '\'':
			i++
		case body[i] == '\'':
			quoted = !quoted
		case body[i] == ',' && !quoted:
			labels = append(labels, strings.TrimSpace(body[start:i]))
			start = i + 1
		}
	}
	if quoted {
		return nil, false
	}
	labels = append(labels, strings.TrimSpace(body[start:]))
	// 只处理标准 SQL 字符串标签。
	for _, label := range labels {
		if len(label) < 2 || label[0] != '\'' || label[len(label)-1] != '\'' {
			return nil, false
		}
	}
	return labels, true
}

// stripLeadingComments 去掉语句开头的注释。
func stripLeadingComments(text string) string {
	for {
		text = strings.TrimSpace(text)
		switch {
		case strings.HasPrefix(text, "--"):
			if idx := strings.IndexByte(text, '\n'); idx >= 0 {
				text = text[idx+1:]
				continue
			}
			return ""
		case strings.HasPrefix(text, "/*"):
			if idx := strings.Index(text, "*/"); idx >= 0 {
				text = text[idx+2:]
				continue
			}
			return ""
		}
		return text
	}
}

// hasPrefixFold 忽略大小写比较前缀。
func hasPrefixFold(text, prefix string) bool {
	return len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix)
}

// splitSQL 按分号切分语句，跳过注释与引号内容。
func splitSQL(content string) []string {
	var statements []string
	var current strings.Builder
	// 0 普通 1 行注释 2 块注释 3 单引号 4 双引号 5 dollar 引用
	state := 0
	dollarTag := ""
	for i := 0; i < len(content); i++ {
		ch := content[i]
		var next byte
		if i+1 < len(content) {
			next = content[i+1]
		}
		switch state {
		case 1:
			current.WriteByte(ch)
			if ch == '\n' {
				state = 0
			}
			continue
		case 2:
			if ch == '*' && next == '/' {
				current.WriteString("*/")
				i++
				state = 0
				continue
			}
			current.WriteByte(ch)
			continue
		case 3:
			if ch == '\'' && next == '\'' {
				current.WriteString("''")
				i++
				continue
			}
			current.WriteByte(ch)
			if ch == '\'' {
				state = 0
			}
			continue
		case 4:
			if ch == '"' && next == '"' {
				current.WriteString(`""`)
				i++
				continue
			}
			current.WriteByte(ch)
			if ch == '"' {
				state = 0
			}
			continue
		case 5:
			if dollarTag != "" && strings.HasPrefix(content[i:], dollarTag) {
				current.WriteString(dollarTag)
				i += len(dollarTag) - 1
				state = 0
				continue
			}
			current.WriteByte(ch)
			continue
		}
		// 普通状态识别注释与引号起点
		if ch == '-' && next == '-' {
			current.WriteString("--")
			i++
			state = 1
			continue
		}
		if ch == '/' && next == '*' {
			current.WriteString("/*")
			i++
			state = 2
			continue
		}
		if ch == '\'' {
			current.WriteByte(ch)
			state = 3
			continue
		}
		if ch == '"' {
			current.WriteByte(ch)
			state = 4
			continue
		}
		if ch == '$' {
			if tag := dollarTagAt(content, i); tag != "" {
				dollarTag = tag
				current.WriteString(tag)
				i += len(tag) - 1
				state = 5
				continue
			}
		}
		if ch == ';' {
			if statement := strings.TrimSpace(current.String()); statement != "" {
				statements = append(statements, statement+";")
			}
			current.Reset()
			continue
		}
		current.WriteByte(ch)
	}
	if statement := strings.TrimSpace(current.String()); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

// dollarTagAt 识别 $$ 或 $tag$ 形式的引用标记。
func dollarTagAt(content string, start int) string {
	end := start + 1
	for end < len(content) {
		ch := content[end]
		if ch == '$' {
			return content[start : end+1]
		}
		if !(ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			return ""
		}
		end++
	}
	return ""
}

// sparkSQL 通过 lark-cli 透传调用 spark SQL 接口，返回查询行。
func (s *Client) sparkSQL(
	ctx context.Context,
	profile, appID, env, sql, work string,
) (rows []map[string]any, resultErr error) {
	started := time.Now()
	operation := "execute"
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
		operation = "query"
	}
	defer func() {
		if resultErr != nil {
			log.Printf("[ddl] sql operation=%s target_id=%s env=%s status=failed duration_ms=%d error=%q",
				operation, appID, env, time.Since(started).Milliseconds(), resultErr)
			return
		}
		log.Printf("[ddl] sql operation=%s target_id=%s env=%s status=success duration_ms=%d rows=%d",
			operation, appID, env, time.Since(started).Milliseconds(), len(rows))
	}()

	binary, err := s.Lark.Binary()
	if err != nil {
		return nil, errors.New("未找到 lark-cli，请安装后刷新")
	}
	// 每个请求使用独立响应文件，避免并发互相覆盖。
	result, err := os.CreateTemp(work, ".sql-result-*.json")
	if err != nil {
		return nil, fmt.Errorf("无法创建 SQL 响应文件：%w", err)
	}
	resultFile := result.Name()
	defer os.Remove(resultFile)
	// 关闭文件后交由 CLI 写入。
	if err := result.Close(); err != nil {
		return nil, fmt.Errorf("无法初始化 SQL 响应文件：%w", err)
	}
	params, _ := json.Marshal(map[string]string{"env": env})
	payload, _ := json.Marshal(map[string]string{"sql": sql})
	args := []string{"api", "POST", "/open-apis/spark/v1/apps/" + appID + "/sql_commands",
		"--as", "user", "--params", string(params), "--data", "-", "--output", filepath.Base(resultFile)}
	if profile != "" {
		args = append(args, "--profile="+profile)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	// SQL 经 stdin 传递，避免 JSON 转义撑大 Windows 命令行。
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Dir, cmd.Env, cmd.WaitDelay = work, larkcli.Environment(), 5*time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	if ctx.Err() != nil {
		return nil, errors.New("SQL 请求超时或已中断")
	}
	if runErr != nil {
		return nil, apiCommandError(runErr, stderr.Bytes(), output)
	}
	// 二进制响应会落到 --output 指定的文件
	raw := output
	if file, err := os.ReadFile(resultFile); err == nil && len(bytes.TrimSpace(file)) > 0 {
		raw = file
	}
	return parseSparkSQL(raw)
}

// parseSparkSQL 解析 spark SQL 响应，展开二次 JSON 编码的结果。
func parseSparkSQL(raw []byte) ([]map[string]any, error) {
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		OK   *bool           `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("无法解析 SQL 接口返回：" + clipText(strings.TrimSpace(string(raw)), 300))
	}
	if envelope.OK != nil && !*envelope.OK {
		return nil, errors.New("SQL 接口返回失败")
	}
	if envelope.Code != 0 {
		message := strings.TrimSpace(envelope.Msg)
		if message == "" {
			message = "SQL 接口返回错误码 " + fmt.Sprint(envelope.Code)
		}
		return nil, errors.New(message)
	}
	var data struct {
		Result json.RawMessage `json:"result"`
	}
	if len(envelope.Data) > 0 {
		_ = json.Unmarshal(envelope.Data, &data)
	}
	return parseSQLResult(data.Result), nil
}

// parseSQLResult 展开 result 中的多层 JSON 字符串。
func parseSQLResult(result json.RawMessage) []map[string]any {
	rows := []map[string]any{}
	if len(result) == 0 {
		return rows
	}
	var text string
	if json.Unmarshal(result, &text) != nil {
		text = string(result)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return rows
	}
	var outer []json.RawMessage
	if json.Unmarshal([]byte(text), &outer) != nil {
		var direct []map[string]any
		if json.Unmarshal([]byte(text), &direct) == nil {
			return direct
		}
		return rows
	}
	for _, item := range outer {
		var encoded string
		if json.Unmarshal(item, &encoded) == nil {
			var batch []map[string]any
			if json.Unmarshal([]byte(encoded), &batch) == nil {
				rows = append(rows, batch...)
			}
			continue
		}
		var batch []map[string]any
		if json.Unmarshal(item, &batch) == nil {
			rows = append(rows, batch...)
		}
	}
	return rows
}

// clipText 截断过长文本，避免异常信息刷屏。
func clipText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

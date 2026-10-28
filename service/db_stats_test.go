package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/atopos31/llmio/models"
)

// 这个文件盯的是**状态页在迁移期间还活着**这件事。起因是真机反馈：
// 迁移跑着的时候点「开始迁移」会 500（`database is locked`），用户得手动刷新
// 才起得来。根因是"开始迁移"那条路为了拿一个文件大小去调了整条 stats 读，
// 而那条读里有全表聚合，撞上迁移的写锁就整个失败。
//
// 所以这里验三条：
//
//  1. 撞锁要**重试**，一次 BUSY 不是终局。
//  2. 真量不到时给 nil（前端画"量不到"），**不报错**、更不拿 0 冒充。
//  3. 全表聚合要**带冷却**：状态页每 2 秒轮询一次，每次都真扫是自造锁竞争。

func resetDBStatsCache() {
	dbStatsMu.Lock()
	defer dbStatsMu.Unlock()
	dbStatsCache, dbStatsAt = nil, time.Time{}
	// 索引就绪标志也是包级的：上一个用例建了索引，这个用例可能正是要验
	// 没有索引时的退路。默认从"没索引"起跑，要用索引的用例自己 Ensure。
	dbStatsIndexesReady.Store(false)
}

func TestDBStats_ReadsTheRealNumbersAndCaches(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	seedPlaintextRows(t, ctx, [][]byte{[]byte("hello"), []byte("world")}, time.Hour)

	first := ReadDBStats(ctx)
	if first == nil {
		t.Fatal("量得到却给了 nil")
	}
	if first.Rows != 2 || first.PendingRows != 2 || first.FramedRows != 0 {
		t.Fatalf("行数不对：rows=%d pending=%d framed=%d", first.Rows, first.PendingRows, first.FramedRows)
	}
	if first.PageSize <= 0 || first.PageCount <= 0 {
		t.Fatalf("页信息不对：page_size=%d page_count=%d", first.PageSize, first.PageCount)
	}
	if first.StatsAt == 0 || first.Stale {
		t.Fatalf("刚量到的数应当是新鲜的：stats_at=%d stale=%v", first.StatsAt, first.Stale)
	}

	// 冷却期内直接给缓存：这一条是"别每次轮询都扫全表"的落点。
	// 用"往库里再插一行"来验——真去扫的话 rows 会变 3，给缓存就还是 2。
	seedPlaintextRows(t, ctx, [][]byte{[]byte("third")}, time.Hour)
	second := ReadDBStats(ctx)
	if second.Rows != 2 {
		t.Fatalf("冷却期内应当给缓存（rows 应仍为 2），实得 %d——全表聚合被每次轮询都真跑了", second.Rows)
	}
	if second.StatsAt != first.StatsAt {
		t.Fatalf("缓存里的 stats_at 不该变：%d → %d", first.StatsAt, second.StatsAt)
	}

	// 冷却期过了就必须真量。把时钟往回拨而不是 sleep 5 秒。
	dbStatsMu.Lock()
	dbStatsAt = time.Now().Add(-dbStatsTTL - time.Second)
	dbStatsMu.Unlock()

	third := ReadDBStats(ctx)
	if third.Rows != 3 {
		t.Fatalf("冷却期过后应当真量（rows 应为 3），实得 %d", third.Rows)
	}
}

// 空闲磁盘那一项：界面拿它**提前算**重整够不够（VACUUM 要 2 倍库大小的可用空间），
// 所以它必须在状态里，而且必须是真的（一次 statfs，不是估算）。
//
// 判据取"大于 0 且小于等于库所在卷的总量"这种弱形状，是因为测试机上的可用空间
// 是个一直在动的数：钉死具体值只会得到一条时灵时不灵的用例。真正要钉的是
// **它有没有被填上**——留 0 的话界面会把每一次重整都判成"空间不够"，
// 而那条路上的用户看到的会是一个永远点不动的按钮。
func TestDBStats_ReportsFreeDiskSpace(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()

	stats := ReadDBStats(context.Background())
	if stats == nil {
		t.Fatal("量得到却给了 nil")
	}
	if stats.DiskFree <= 0 {
		t.Fatalf("空闲磁盘没量到（%d）——界面据此判重整的空间够不够，0 会把每一次都判成不够",
			stats.DiskFree)
	}
	// 它必须够放得下这份刚建出来的小库：这条用例自己就活在同一个卷上。
	if !enoughDisk(stats.DiskFree, stats.FileSize) {
		t.Fatalf("同一卷上量出的空闲 %d 放不下库 %d——这两个数里有一个不是这个卷的",
			stats.DiskFree, stats.FileSize)
	}
}

// 撞锁要重试。这里不真去制造 SQLITE_BUSY（那要另一条连接持写锁，慢且脆），
// 而是直接验判据本身：判错了的后果是"不重试"，退化方向是安全的。
func TestDBStats_BusyErrorDetection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"驱动错误码 SQLITE_BUSY", codedErr(5), true},
		{"驱动错误码 SQLITE_LOCKED", codedErr(6), true},
		{"只有文本（换过驱动也认得出）", errors.New("database is locked (5) (SQLITE_BUSY)"), true},
		{"包了一层也要认得出", fmt.Errorf("读库现状: %w", codedErr(5)), true},
		{"约束冲突不是撞锁", codedErr(19), false},
		{"语法错不是撞锁", errors.New("near \"SELEC\": syntax error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsBusyErr(c.err); got != c.want {
				t.Fatalf("IsBusyErr(%v) = %v，期望 %v", c.err, got, c.want)
			}
		})
	}
}

// 真量不到（库都关了）时：给 nil，不 panic、不报错、不拿 0 冒充。
func TestDBStats_UnreadableReturnsNilNotZeros(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()

	ctx := context.Background()
	sqlDB, err := models.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	if got := ReadDBStats(ctx); got != nil {
		t.Fatalf("量不到应当给 nil（前端据此画「量不到」），实得 %+v", got)
	}
	// 再问一次也不该 panic，也不该突然冒出一组 0。
	if got := ReadDBStats(ctx); got != nil {
		t.Fatalf("第二次仍然该是 nil，实得 %+v", got)
	}
}

// 量得到 → 后来量不到：给**上一次的**并标 stale，而不是把整块收回去。
// 这中间的差别在界面上是"数字旧了几分钟"和"什么都没有"。
func TestDBStats_ServesLastKnownWhenMeasurementFails(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	seedPlaintextRows(t, ctx, [][]byte{[]byte("hello")}, time.Hour)
	good := ReadDBStats(ctx)
	if good == nil {
		t.Fatal("第一次应当量得到")
	}

	// 让缓存过期，再把库关掉——下一次真量必然失败。
	dbStatsMu.Lock()
	dbStatsAt = time.Now().Add(-dbStatsTTL - time.Second)
	dbStatsMu.Unlock()
	sqlDB, _ := models.DB.DB()
	_ = sqlDB.Close()

	got := ReadDBStats(ctx)
	if got == nil {
		t.Fatal("量不到时应当拿上一次的顶一顶，实得 nil")
	}
	if !got.Stale {
		t.Fatal("顶替的那一份必须标 stale——不标的话，刚 VACUUM 完的人会以为没生效")
	}
	if got.Rows != good.Rows {
		t.Fatalf("顶替的应当是上一次那份数，rows=%d 期望 %d", got.Rows, good.Rows)
	}
}

// 上一次的数太老就不给了：刚 VACUUM 完却显示十分钟前的大小，
// 比不显示更容易让人做错判断。
func TestDBStats_TooOldCacheIsNotServed(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	seedPlaintextRows(t, ctx, [][]byte{[]byte("hello")}, time.Hour)
	if ReadDBStats(ctx) == nil {
		t.Fatal("第一次应当量得到")
	}

	dbStatsMu.Lock()
	dbStatsAt = time.Now().Add(-dbStatsMaxStale - time.Second)
	dbStatsMu.Unlock()
	sqlDB, _ := models.DB.DB()
	_ = sqlDB.Close()

	if got := ReadDBStats(ctx); got != nil {
		t.Fatalf("超过 maxStale 的旧数不该再给，实得 %+v", got)
	}
}

// 调用方拿到的是副本：它会在 DTO 上写 Stale，不能改到缓存本体。
func TestDBStats_CallersGetTheirOwnCopy(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	seedPlaintextRows(t, ctx, [][]byte{[]byte("hello")}, time.Hour)
	first := ReadDBStats(ctx)
	first.Stale = true
	first.Rows = 999

	second := ReadDBStats(ctx)
	if second.Stale || second.Rows == 999 {
		t.Fatal("缓存被调用方改掉了")
	}
}

// 两条全表聚合必须**只读索引**——这是线上那条 `context deadline exceeded`
// 的修复落点，所以这里盯三件事：
//
//  1. 建索引之后，真正发出去的 SQL 的计划里就是那两条表达式索引。
//  2. 走索引量出来的数与退路（全表扫）逐项相等，不是"快了但数错了"。
//  3. 两条索引都是表达式索引（含 partial 条件），不会被同名但定义不同的
//     存量索引顶替掉——那种顶替不会报错，只会让读静默退回全表扫。
func TestDBStats_EnsureIndexesPointsAggregatesAtIndexes(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	if err := EnsureDBStatsIndexes(ctx); err != nil {
		t.Fatalf("建索引失败：%v", err)
	}
	if !dbStatsIndexesReady.Load() {
		t.Fatal("索引建成了，就绪标志却没置上——读会白白退回全表扫")
	}

	rowQ, groupQ := dbStatsQueries()
	if plan := queryPlan(t, ctx, rowQ); !strings.Contains(plan, dbStatsChatIOIndexName) {
		t.Fatalf("chat_ios 聚合没走 %s，计划是：%s", dbStatsChatIOIndexName, plan)
	}
	if plan := queryPlan(t, ctx, groupQ); !strings.Contains(plan, "USING COVERING INDEX "+dbStatsGroupLenIndexName) {
		t.Fatalf("block_groups 聚合没走覆盖索引 %s，计划是：%s", dbStatsGroupLenIndexName, plan)
	}

	// 可重复调用：第二次不该再动任何东西，更不该报错。
	if err := EnsureDBStatsIndexes(ctx); err != nil {
		t.Fatalf("第二次建索引应当是无操作，实得 %v", err)
	}
}

// 同一个库、同一批数据：走索引与走退路必须量出**一模一样**的数。
//
// 表达式索引由 SQLite 自己维护，理论上不会错；但它错的时候是"数悄悄变了"，
// 而状态页的这组数正是迁移进度的唯一读数——比慢更可怕。
func TestDBStats_IndexBackedNumbersMatchFallback(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	ids := seedPlaintextRows(t, ctx, [][]byte{bigRow(1, 8<<10), bigRow(2, 8<<10), bigRow(3, 8<<10)}, time.Hour)
	if _, err := RunLogCompress(ctx, false); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	// 造出"请求体已压、响应体还是明文"的中间态，让 pending 与 framed 同时非零。
	writeRawColumn(t, ctx, ids[0], "of_string", "响应体明文")

	fallback := ReadDBStats(ctx)
	if fallback == nil {
		t.Fatal("退路量不到数")
	}

	dbStatsMu.Lock()
	dbStatsAt = time.Time{}
	dbStatsMu.Unlock()
	if err := EnsureDBStatsIndexes(ctx); err != nil {
		t.Fatalf("建索引失败：%v", err)
	}
	indexed := ReadDBStats(ctx)
	if indexed == nil {
		t.Fatal("走索引量不到数")
	}
	for name, pair := range map[string][2]int64{
		"行数":    {fallback.Rows, indexed.Rows},
		"待迁移行数": {fallback.PendingRows, indexed.PendingRows},
		"已压行数":  {fallback.FramedRows, indexed.FramedRows},
		"块行数":   {fallback.BlockRows, indexed.BlockRows},
		"组行数":   {fallback.GroupRows, indexed.GroupRows},
		"组字节数":  {fallback.GroupBytes, indexed.GroupBytes},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s：退路 %d，走索引 %d——两条路的数必须相同", name, pair[0], pair[1])
		}
	}
	if indexed.FramedRows == 0 || indexed.PendingRows == 0 {
		t.Fatalf("语料没造出中间态（pending=%d framed=%d），这条用例什么也没验",
			indexed.PendingRows, indexed.FramedRows)
	}

	// 索引要跟着**三种写**一起维护，否则界面上那个数会一直停在建索引那一刻。
	seedPlaintextRows(t, ctx, [][]byte{[]byte("新来的一行")}, time.Hour)
	writeRawColumn(t, ctx, ids[1], "of_string", "又一条明文")
	// 软删：partial 索引的 WHERE 条件正是这一列，删掉的行必须从索引里消失。
	if err := models.DB.WithContext(ctx).Exec(
		`UPDATE chat_ios SET deleted_at = ? WHERE id = ?`, time.Now(), ids[2]).Error; err != nil {
		t.Fatalf("软删失败：%v", err)
	}

	dbStatsMu.Lock()
	dbStatsAt = time.Time{}
	dbStatsMu.Unlock()
	after := ReadDBStats(ctx)
	if after == nil {
		t.Fatal("写入之后量不到数")
	}
	if after.Rows != indexed.Rows {
		// 3 行 + 新增 1 行 - 软删 1 行 = 3 行：与原来相等，所以单独钉住它。
		t.Fatalf("rows=%d，期望 %d（插一行、软删一行）", after.Rows, indexed.Rows)
	}
	if after.PendingRows != indexed.PendingRows+2 {
		t.Fatalf("pending=%d，期望 %d（新增一行 + 一列翻回明文）", after.PendingRows, indexed.PendingRows+2)
	}
}

// 同名但定义不对的存量索引必须被换掉。
//
// `CREATE INDEX IF NOT EXISTS` 只看名字：一条 `idx_chat_ios_forms(log_id)`
// 会让建索引"成功"，而读继续全表扫——没有报错、没有慢日志，
// 只有 6 秒预算再次被吃光。这里先造出这种错索引再 Ensure。
func TestDBStats_EnsureIndexesRepairsWrongDefinition(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	wrong := []string{
		`CREATE INDEX idx_chat_ios_forms ON chat_ios (log_id)`,
		`CREATE INDEX idx_block_groups_len ON block_groups (raw_len)`,
	}
	for _, s := range wrong {
		if err := models.DB.WithContext(ctx).Exec(s).Error; err != nil {
			t.Fatalf("造错索引失败：%v", err)
		}
	}
	if ok, err := dbStatsIndexMatches(ctx, dbStatsChatIOIndexName, "chat_ios", 3, true); err != nil || ok {
		t.Fatalf("前置：错索引不该被判为合格（ok=%v err=%v）", ok, err)
	}

	if err := EnsureDBStatsIndexes(ctx); err != nil {
		t.Fatalf("修索引失败：%v", err)
	}
	for _, spec := range dbStatsIndexSpecs {
		ok, err := dbStatsIndexMatches(ctx, spec.name, spec.table, spec.columns, spec.partial)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("%s 没有被换成表达式索引——读会静默退回全表扫", spec.name)
		}
	}
	// 换成真的表达式索引之后，计划里必须见到它。
	rowQ, _ := dbStatsQueries()
	if plan := queryPlan(t, ctx, rowQ); !strings.Contains(plan, dbStatsChatIOIndexName) {
		t.Fatalf("修完之后计划仍然不对：%s", plan)
	}
}

// queryPlan 把 `EXPLAIN QUERY PLAN` 的每一步拼成一行，供断言。
func queryPlan(t *testing.T, ctx context.Context, q string) string {
	t.Helper()
	rows, err := models.DB.WithContext(ctx).Raw("EXPLAIN QUERY PLAN " + q).Rows()
	if err != nil {
		t.Fatalf("EXPLAIN 失败：%v", err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("读计划失败：%v", err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读计划失败：%v", err)
	}
	return strings.Join(steps, " | ")
}

// codedErr 造一个带 SQLite 错误码的 error，形状与驱动一致（*Error 有 Code()）。
type codedErr int

func (e codedErr) Error() string { return fmt.Sprintf("sqlite error (code %d)", int(e)) }
func (e codedErr) Code() int     { return int(e) }

// 待迁移的行数按**三列**算，不按请求体一列算。
//
// 按单列算的话，"请求体迁完了、响应体还没"这种最常见的中间态会读成 0 行——
// 而它正是真机上发生过的：界面报"已完成"，1.36 GiB 明文一行没动。
// 这个口径与迁移自己的候选集必须一致：两个数说的是同一件事，分头写迟早会漂。
func TestDBStats_PendingSpansAllThreeColumns(t *testing.T) {
	setupLogCompressTestDB(t)
	resetDBStatsCache()
	ctx := context.Background()

	ids := seedPlaintextRows(t, ctx, [][]byte{bigRow(1, 8<<10)}, time.Hour)
	if _, err := RunLogCompress(ctx, false); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}

	// 请求体已压成帧、响应体还是明文：只看 input 的话这里 pending 是 0。
	plain := "响应体明文"
	writeRawColumn(t, ctx, ids[0], "of_string", plain)

	got := ReadDBStats(ctx)
	if got == nil {
		t.Fatal("量得到却给了 nil")
	}
	if got.PendingRows != 1 {
		t.Fatalf("pending_rows=%d，期望 1——响应体那一列没被算进候选", got.PendingRows)
	}
	if got.FramedRows != 1 {
		t.Fatalf("framed_rows=%d，期望 1——请求体那一列已经是帧了", got.FramedRows)
	}
	// 「这两列此刻占多少字节」**故意不在这里**：那个数只能靠读载荷算
	// （`sum(length(CAST(列 AS BLOB)))`，真机 1.36 GiB 要 0.80 秒），
	// 而这个接口每 2 秒被轮询一次，长读事务在 `journal_mode=delete` 下挡写。
	// 界面要的"库现在多大"由 file_size 回答。详见 db_stats.go 的字段说明。
	if got.FileSize <= 0 {
		t.Fatalf("file_size=%d，界面靠它报「实际占用」", got.FileSize)
	}
}

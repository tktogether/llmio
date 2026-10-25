package models

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// closeOnCleanup 注册在测试结束时关闭连接。
//
// 必须有这一步：Windows 上文件被占用时 t.TempDir 的清理会失败。
// t.Cleanup 为 LIFO，TempDir 的清理最早注册因而最后执行，
// 所以此处注册的关闭一定跑在它之前。
func closeOnCleanup(t *testing.T, db *gorm.DB) {
	t.Helper()
	t.Cleanup(func() {
		if db == nil {
			return
		}
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}

func TestInit_BackfillsAuthKeyIOLogToFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}
	closeOnCleanup(t, db)

	if err := db.Exec(`
		CREATE TABLE auth_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			name TEXT,
			key TEXT,
			status NUMERIC,
			allow_all NUMERIC,
			models TEXT,
			expires_at DATETIME,
			usage_count INTEGER,
			last_used_at DATETIME
		)
	`).Error; err != nil {
		t.Fatalf("failed to create legacy auth_keys table: %v", err)
	}

	if err := db.Exec(`INSERT INTO auth_keys (name, key, status, allow_all, usage_count) VALUES (?, ?, ?, ?, ?)`,
		"legacy-project", "legacy-key", true, true, 0,
	).Error; err != nil {
		t.Fatalf("failed to seed legacy auth key: %v", err)
	}

	Init(context.Background(), path)
	// Init 会另外打开一个连到同一文件的连接并写入包级 DB，同样需要关闭
	closeOnCleanup(t, DB)

	authKey, err := gorm.G[AuthKey](DB).Where("key = ?", "legacy-key").First(context.Background())
	if err != nil {
		t.Fatalf("failed to load migrated auth key: %v", err)
	}

	if authKey.IOLog == nil {
		t.Fatal("expected io_log to be backfilled")
	}
	if *authKey.IOLog {
		t.Fatal("expected io_log default to false")
	}
}

// chat_logs.backup 是后加的非指针 bool：老库经 AutoMigrate 加列后存量行为 NULL，
// 不归一的话日志列表扫描 bool 会直接报错。这条测试去掉 init.go 里的回填就会红。
func TestInit_BackfillsChatLogBackupToFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}
	closeOnCleanup(t, db)

	// 老 chat_logs 表：没有 backup 列
	if err := db.Exec(`
		CREATE TABLE chat_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			name TEXT,
			trace_id TEXT,
			provider_model TEXT,
			provider_name TEXT,
			status TEXT,
			style TEXT,
			chat_io NUMERIC,
			auth_key_id INTEGER
		)
	`).Error; err != nil {
		t.Fatalf("failed to create legacy chat_logs table: %v", err)
	}

	if err := db.Exec(`INSERT INTO chat_logs (name, provider_name, status, auth_key_id) VALUES (?, ?, ?, ?)`,
		"legacy-model", "legacy-provider", "success", 0,
	).Error; err != nil {
		t.Fatalf("failed to seed legacy chat log: %v", err)
	}

	Init(context.Background(), path)
	closeOnCleanup(t, DB)

	chatLog, err := gorm.G[ChatLog](DB).Where("name = ?", "legacy-model").First(context.Background())
	if err != nil {
		t.Fatalf("failed to load migrated chat log（backup 为 NULL 时这里会因 bool 扫描失败而报错）: %v", err)
	}
	if chatLog.Backup {
		t.Fatal("expected backup default to false")
	}
}

// model_with_providers.backup 同理：NULL 的备用标记在调度分池里读不出来。
func TestInit_BackfillsModelWithProviderBackupToFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}
	closeOnCleanup(t, db)

	if err := db.Exec(`
		CREATE TABLE model_with_providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			model_id INTEGER,
			provider_id INTEGER,
			provider_model TEXT,
			weight INTEGER
		)
	`).Error; err != nil {
		t.Fatalf("failed to create legacy model_with_providers table: %v", err)
	}

	if err := db.Exec(`INSERT INTO model_with_providers (model_id, provider_id, provider_model, weight) VALUES (?, ?, ?, ?)`,
		1, 1, "legacy-model", 1,
	).Error; err != nil {
		t.Fatalf("failed to seed legacy association: %v", err)
	}

	Init(context.Background(), path)
	closeOnCleanup(t, DB)

	mp, err := gorm.G[ModelWithProvider](DB).Where("provider_model = ?", "legacy-model").First(context.Background())
	if err != nil {
		t.Fatalf("failed to load migrated association（backup 为 NULL 时这里会因 bool 扫描失败而报错）: %v", err)
	}
	if mp.Backup == nil {
		t.Fatal("expected backup to be backfilled")
	}
	if *mp.Backup {
		t.Fatal("expected backup default to false")
	}
}

// TestInit_DedupsAssociationsAndCreatesUniqueIndex：存量重复行清洗（每组
// 留最小 id、软删其余）与 partial 唯一索引同在一条迁移链上。掉了任何一环，
// 批量创建"撞索引转 skipped"与"删了再建"都会失去底座。
func TestInit_DedupsAssociationsAndCreatesUniqueIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}
	closeOnCleanup(t, db)
	if err := db.AutoMigrate(&ModelWithProvider{}); err != nil {
		t.Fatalf("failed to migrate associations: %v", err)
	}

	// 存量脏数据：同 (model, provider, provider_model) 两行 + 一行正常
	seeds := []ModelWithProvider{
		{ModelID: 1, ProviderID: 1, ProviderModel: "dup", Status: new(false)},
		{ModelID: 1, ProviderID: 1, ProviderModel: "dup", Status: new(false)},
		{ModelID: 1, ProviderID: 1, ProviderModel: "keep", Status: new(false)},
	}
	for i := range seeds {
		seeds[i].ID = uint(i + 1)
	}
	// 显式主键批量插入：分着建，避免 GORM 把同结构体切片当一条批处理吞掉显式 ID
	for i := range seeds {
		if err := db.Create(&seeds[i]).Error; err != nil {
			t.Fatalf("failed to seed row %d: %v", i, err)
		}
	}

	Init(context.Background(), path)
	closeOnCleanup(t, DB)

	ctx := context.Background()

	// 每组留最小 id：1 号存活，2 号被软删，3 号不动
	var alive1, alive2, alive3 int64
	DB.Model(&ModelWithProvider{}).Where("id = ?", 1).Count(&alive1)
	DB.Model(&ModelWithProvider{}).Where("id = ?", 2).Count(&alive2)
	DB.Model(&ModelWithProvider{}).Where("id = ?", 3).Count(&alive3)
	if alive1 != 1 || alive2 != 0 || alive3 != 1 {
		t.Fatalf("存活状态 id1=%d id2=%d id3=%d，期望 1/0/1（每组留最小 id）", alive1, alive2, alive3)
	}

	// 索引生效：重复三元组建不进去
	err = DB.WithContext(ctx).Create(&ModelWithProvider{ModelID: 1, ProviderID: 1, ProviderModel: "dup", Status: new(false)}).Error
	if err == nil {
		t.Fatal("重复三元组应撞唯一索引被拒")
	}

	// partial：软删后同三元组可重建——"删了再建"不需要先物理清尸
	if err := DB.WithContext(ctx).Delete(&ModelWithProvider{}, 3).Error; err != nil {
		t.Fatalf("软删 3 号：%v", err)
	}
	if err := DB.WithContext(ctx).Create(&ModelWithProvider{ModelID: 1, ProviderID: 1, ProviderModel: "keep", Status: new(false)}).Error; err != nil {
		t.Fatalf("软删后重建同三元组不该被索引拒绝：%v", err)
	}
}

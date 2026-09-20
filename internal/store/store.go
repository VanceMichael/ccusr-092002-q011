// Package store 管理 SQLite 连接、迁移与时间来源。
package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vancemichael/092002-forest-brand-attest/migrations"
)

type Store struct {
	DB  *sql.DB
	now func() time.Time
}

// Open 打开（不存在则创建）数据库文件并应用全部迁移。
func Open(path string) (*Store, error) {
	// 多个 _pragma 必须以 & 连接（url.Values 会用逗号合并同名键）。
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	s := &Store{DB: db, now: time.Now}
	if err := s.Migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Now() time.Time { return s.now() }

// SetClock 仅供测试固定服务端时间。
func (s *Store) SetClock(clock func() time.Time) {
	if clock == nil {
		s.now = time.Now
		return
	}
	s.now = clock
}

// Migrate 按文件名顺序应用尚未记录的内嵌迁移。
func (s *Store) Migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
	    version TEXT PRIMARY KEY,
	    applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return err
	}

	applied := map[string]bool{}
	rows, err := s.DB.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		content, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("迁移 %s 失败: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO schema_migrations(version) VALUES (?)`, version,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

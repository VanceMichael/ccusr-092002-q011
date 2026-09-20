// migrate 命令对 DATABASE_PATH 指向的数据库应用全部内嵌迁移；服务启动时也会自动迁移。
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

func main() {
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "data/app.sqlite3"
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		log.Fatalf("迁移失败: %v", err)
	}
	if err := st.Close(); err != nil {
		log.Fatalf("关闭数据库失败: %v", err)
	}
	log.Printf("迁移完成：%s", dbPath)
}

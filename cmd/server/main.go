package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/vancemichael/092002-forest-brand-attest/internal/httpapi"
	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "data/app.sqlite3"
	}
	if dir := dirOf(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("创建数据目录失败: %v", err)
		}
	}

	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	log.Printf("标签核销服务启动，监听 :%s，数据库 %s", port, dbPath)
	log.Fatal(http.ListenAndServe(":"+port, httpapi.NewRouter(st)))
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == os.PathSeparator {
			return path[:i]
		}
	}
	return ""
}

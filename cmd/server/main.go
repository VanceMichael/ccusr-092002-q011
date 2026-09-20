package main

import (
	"log"
	"net/http"
	"os"

	"github.com/vancemichael/092002-forest-brand-attest/internal/domain"
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

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	svc := domain.New(st)
	log.Printf("标签码领用与核销服务启动，监听 :%s，数据库 %s", port, dbPath)
	log.Fatal(http.ListenAndServe(":"+port, httpapi.Router(st, svc)))
}

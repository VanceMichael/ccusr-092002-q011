// Package migrations 内嵌数据库迁移 SQL，供服务启动时自动应用。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

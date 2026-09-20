// Package app 通过 go:embed 携带 SQL 迁移，便于单文件部署并与 migrations 目录保持单一事实来源。
package app

import "embed"

// Migrations 内嵌 migrations 目录下全部 SQL 迁移文件。
//
//go:embed migrations/*.sql
var Migrations embed.FS

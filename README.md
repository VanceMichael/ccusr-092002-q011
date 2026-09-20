# 林产品公共品牌授权验真

区域公共品牌标志授权与生产主体、产品规格和检测材料关联，展会展品信息不代表全系列产品获准使用标志。

本服务在授权验真之上实现**标签码领用与核销后端**：把企业现有授权凭证作为不可修改的外部依据，按印厂印制批次、交付箱件、企业领用、实际贴附产品与作废回收，记录每个验证编号的去向；离线设备重传不会让一个码贴到两个批次；授权到期不否定此前售出的合规板材，但未领用的剩余标签会被停用。采购商扫码看到当前产品规格与异常标识，品牌方能查出整批遗失标签最后由谁保管。

本服务采用 HTTP 接口和 SQLite 本地文件（纯 Go 驱动，无需 CGO）。运行参数 `PORT` 指定监听端口，`DATABASE_PATH` 指定数据文件；迁移 SQL 内嵌于程序，启动时自动应用。`fixtures/example.json` 保存不含真实身份的交换示例，`contracts/entities.json` 记录字段约定，`docs/domain.md` 介绍领域规则与状态机。

## 本地开发

`make migrate` 可用 sqlite3 CLI 预初始化数据文件（可选，服务启动会自动迁移），`make test` 运行自动化检查，`make run` 启动服务。`docker compose up --build` 可以启动隔离容器，`APP_PORT` 可调整宿主机端口。

## 编号生命周期

```
印制批次 printed → 装箱 packed → 交付 delivered → 企业领用 issued → 贴附产品 attached
                                   │                  │              │
                                   ▼                  ▼              ▼
                              blocked(到期未领用)   lost(挂失) ◀── 任一环节可挂失 → void(作废回收)
```

## HTTP 接口

所有写入接口建议带 `Idempotency-Key` 头（同键同负载重放首次结果，同键不同负载返回 409）；操作人由 `X-Actor` 头记录。时间字段均为带时区偏移的 ISO 8601。

| 方法 | 路径 | 说明 |
| ---- | ---- | ---- |
| POST | `/v1/admin/attestations` | 导入授权凭证（只追加，不可改删） |
| POST | `/v1/admin/print-batches` | 印厂登记印制批次与预制编号 |
| POST | `/v1/admin/cartons` | 编号装箱 |
| POST | `/v1/admin/deliveries` | 箱件交付企业 |
| POST | `/v1/admin/issues` | 企业按有效授权凭证领用（锁规格） |
| POST | `/v1/admin/product-batches` | 登记实际生产批次 |
| POST | `/v1/attachments` | 离线设备贴附（`client_event_id` 幂等） |
| POST | `/v1/admin/codes/{code}/lost` | 报告遗失（冻结最后保管方） |
| POST | `/v1/admin/codes/{code}/recover` | 找回，恢复挂失前状态 |
| POST | `/v1/admin/codes/{code}/void` | 作废回收 |
| POST | `/v1/admin/block-expired-unused` | 停用无有效授权企业的未领用标签 |
| GET | `/v1/verify/{code}` | 采购商公开验证（规格 + 异常标识） |
| GET | `/v1/admin/codes/{code}/trace` | 单码完整去向链 |
| GET | `/v1/admin/print-batches/{id}/lost-custodians` | 整批遗失最后保管方汇总 |

### 快速验证

```bash
curl -s localhost:8080/v1/verify/CXLP-0001 | jq
```

已贴附合规产品返回 `valid:true` 与当前规格；遗失/停用/作废/未贴附/不存在均带 `abnormal:true` 与机器可读 `reasons`，未贴附编号不返回企业名称。

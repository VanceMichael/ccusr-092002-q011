# 林产品公共品牌授权验真

区域公共品牌标志授权与生产主体、产品规格和检测材料关联，展会展品信息不代表全系列产品获准使用标志。

本服务为标签码领用与核销后端：把企业现有授权凭证作为**不可修改的外部依据**，按印厂印制批次、交付箱件、企业领用、实际贴附产品、作废/回收/挂失记录每个验证编号的去向；采购商扫码看到当前产品规格与异常标识，品牌方可追查整批遗失标签的最后保管人。

采用 HTTP 接口和 SQLite 本地文件（纯 Go 驱动，静态编译）。`PORT` 指定监听端口，`DATABASE_PATH` 指定数据文件（迁移通过 `go:embed` 随二进制分发，启动自动应用）；`fixtures/example.json` 保存不含真实身份的交换示例，`contracts/entities.json` 记录字段约定，`docs/domain.md` 介绍领域规则。

## 标签生命周期

```
printed ──packed──► delivered ──issued──► applied        （正常去向）
   │          │           │             │
   └──────────┴─────┬─────┴─────────────┴──► frozen / void   （异常终态）
                     └─ lost_flag 叠加标记（recovered 可解除）
```

- 编号全局唯一，属于唯一印制批次；状态只进不退，全部变化写入只追加事件台账 `label_events`。
- 离线贴附以 `(device_id, device_event_id)` 幂等：原样重放安全，改贴别的产品批次返回 `409`。
- 授权到期只冻结**未贴附**标签（含已领用与仅交付）；到期前已贴附售出板材验真仍有效。

## 接口

| 方法 & 路径 | 说明 |
| --- | --- |
| `POST /admin/authorizations` | 收录授权凭证（只读；同内容重放幂等，改内容 `409`） |
| `POST /admin/print-batches` | 登记印制批次与编号清单 |
| `POST /admin/boxes` | 编号封入交付箱件 |
| `POST /admin/boxes/{boxID}/deliver` | 整箱交付企业 |
| `POST /admin/labels/issue` | 企业凭有效凭证领用 |
| `POST /devices/apply` | 离线设备回传贴附事件 |
| `POST /admin/labels/lost` / `/recovered` | 挂失 / 寻回 |
| `POST /admin/labels/void` | 作废（`recovered:true` 为回收核销） |
| `POST /admin/maintenance/freeze-expired` | 冻结到期授权范围的未用标签 |
| `GET /verify/{code}` | 公众验真：规格、企业、产品批次与异常标识 |
| `GET /admin/labels/{code}/trace` | 单码全链路台账 |
| `GET /admin/print-batches/{batchID}/trace` | 整批去向与挂失编号最后保管人 |

错误码映射：`400` 字段/状态前置不满足，`404` 编号或单据不存在，`409` 与既有状态冲突（重复贴附、终态后操作、凭证被篡改等）。

## 本地开发

`make migrate` 初始化数据文件（执行内嵌迁移；服务启动时同样会自动迁移），`make test` 运行自动化检查，`make run` 启动服务。`docker compose up --build` 启动隔离容器，`APP_PORT` 调整宿主机端口。

### 最小流程示例

```bash
curl -XPOST localhost:8080/admin/authorizations -d '{
  "credential_id":"CRED-1","producer_ref":"MAKER-DEMO","producer_name":"示范木业（虚构）",
  "product_ref":"SPEC-PLY-18","spec_name":"18mm 杉木生态板",
  "inspection_digest":"sha256:demo","valid_until":"2026-12-31T23:59:59+08:00"}'
curl -XPOST localhost:8080/admin/print-batches -d '{
  "batch_id":"PB-1","printer_ref":"PRESS-1","product_ref":"SPEC-PLY-18",
  "spec_name":"18mm 杉木生态板","codes":["L-1"]}'
curl localhost:8080/verify/L-1   # 未贴附：valid=false, anomaly_code=not_applied
```

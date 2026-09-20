# 领域资料

区域公共品牌标志授权与生产主体、产品规格和检测材料关联，展会展品信息不代表全系列产品获准使用标志。

`contracts/entities.json` 保存外部数据的交换字段与领域规则。来源编号（`source_ref`）仅表示提交方对同一记录的识别符，示例文件不包含真实个人资料。运行时持久化文件由 `DATABASE_PATH` 决定；时间字段使用带时区偏移的 ISO 8601（RFC 3339）字符串，服务端统一归一化为 UTC 存储。

## 标签码去向链

印厂按获准规格预制带验证编号的板材标签后，每个编号沿唯一链路流转，状态机为：

```
printed ──装箱──▶ packed ──交付──▶ delivered ──领用──▶ issued ──贴附──▶ attached
                     │                │                │
                     ▼                ▼                ▼
                   (交付后属企业)  blocked(授权失效)  lost / void
                                     │
                                   lost ◀── 任一未终止状态可挂失；recover 回到挂失前状态
                                     └──▶ void（作废回收）
```

每次状态变化在 `custody_events` 追加一条只增不改的保管事件（保管方类型 `printer` / `producer` / `on_product` / `brand` / `unknown`）。

### 关键规则

1. **授权凭证是不可修改的外部依据。** `attestations` 只允许插入，数据库触发器禁止 UPDATE/DELETE；同一 `(producer_ref, source_ref)` 重复导入报冲突而非覆盖。领用、产品批次登记、贴附都引用凭证 id，系统只比对、不改写。
2. **领用按获准规格。** 领用必须引用该企业在有效期内的凭证，并把规格锁定到编号（`spec_locked`）。企业只能领用已交付给自己的箱件中的编号。
3. **离线设备重传不能让一个码贴到两个批次。** 贴附必须带设备事件号 `client_event_id`（唯一）：同号重放回放在先结果、不新增记录；同号用于不同编号/批次直接冲突。即便换用新事件号，已贴附编号再贴另一批次也被状态机拒绝。
4. **到期不追溯，剩余标签停用。** 贴附的合规判定以设备上报的实际贴附时间 `occurred_at` 为准：贴附在凭证有效期内的已售板材，凭证日后到期仍验真通过（响应带 `attestation_expired_after_attach` 提示）。`POST /v1/admin/block-expired-unused` 只停用“已交付未领用、且该企业已无任何有效授权”的编号（`delivered → blocked`），已领用/已贴附编号不受影响；停用码禁止再领用或贴附。
5. **公开验证不提前暴露企业名。** 只有 `attached` 状态才返回企业、产品与规格；`printed/packed/delivered/issued` 提示标签尚未对应在售产品且不显示企业，防止遗失/挪用标签借扫码页“自证”；`lost/blocked/void/unknown` 均为异常。
6. **遗失可追到最后保管方。** 挂失时冻结当时的保管方到 `lost_reports`，品牌方可按印制批次汇总（`GET /v1/admin/print-batches/{id}/lost-custodians`），回答“整批遗失标签最后由谁保管”；单码完整去向见 `GET /v1/admin/codes/{code}/trace`。

-- 标签码领用与核销：授权凭证（只追加外部依据）→ 印制批次 → 箱件 → 交付 → 领用 → 产品批次 → 贴附 → 挂失/作废
-- 时间字段统一为带时区偏移的 ISO 8601 字符串（RFC 3339）。

-- 企业现有授权凭证：只追加的外部依据，导入后不可修改、不可删除（见文末触发器）。
CREATE TABLE IF NOT EXISTS attestations (
    id                TEXT PRIMARY KEY,
    source_ref        TEXT NOT NULL,              -- 外部授权系统对该凭证的记录号
    producer_ref      TEXT NOT NULL,
    product_ref       TEXT NOT NULL,
    batch_ref         TEXT NOT NULL,              -- 凭证关联的授权/展品批次引用
    spec              TEXT NOT NULL,              -- 获准的产品规格
    inspection_digest TEXT NOT NULL,              -- 检测材料摘要
    valid_from        TEXT,                       -- 为空表示不约束起始
    valid_until       TEXT NOT NULL,
    imported_at       TEXT NOT NULL,
    UNIQUE(producer_ref, source_ref)
);

-- 印厂印制批次
CREATE TABLE IF NOT EXISTS print_batches (
    id          TEXT PRIMARY KEY,
    printer_ref TEXT NOT NULL,
    note        TEXT,
    printed_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- 交付箱件
CREATE TABLE IF NOT EXISTS cartons (
    id             TEXT PRIMARY KEY,
    print_batch_id TEXT NOT NULL REFERENCES print_batches(id),
    delivery_id    TEXT,                          -- 交付后回填
    note           TEXT,
    created_at     TEXT NOT NULL
);

-- 标签编号主表：status 为当前状态机位置，current_custodian_* 为当前保管方。
CREATE TABLE IF NOT EXISTS codes (
    code                     TEXT PRIMARY KEY,    -- 验证编号（二维码内容）
    print_batch_id           TEXT NOT NULL REFERENCES print_batches(id),
    serial                   TEXT NOT NULL,
    status                   TEXT NOT NULL CHECK(status IN (
                               'printed',   -- 已印制，印厂保管
                               'packed',    -- 已装箱
                               'delivered', -- 箱件已交付企业，尚未领用
                               'issued',    -- 企业已领用，尚未贴附
                               'attached',  -- 已贴附到具体产品批次
                               'lost',      -- 报告遗失（prior_status 记录挂失前状态）
                               'blocked',   -- 授权失效后未领用标签被停用
                               'void'       -- 作废回收
                             )),
    prior_status             TEXT,
    carton_id                TEXT REFERENCES cartons(id),
    holder_producer_ref      TEXT,                -- 交付/领用后的持有企业
    issue_id                 TEXT,
    spec_locked              TEXT,                -- 领用时按授权锁定的规格
    attached_batch_id        TEXT,
    current_custodian_type   TEXT NOT NULL,       -- printer / producer / on_product / brand
    current_custodian_ref    TEXT NOT NULL,
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL,
    UNIQUE(print_batch_id, serial)
);

-- 箱件交付单
CREATE TABLE IF NOT EXISTS deliveries (
    id           TEXT PRIMARY KEY,
    producer_ref TEXT NOT NULL,
    receiver_ref TEXT,
    delivered_at TEXT NOT NULL,
    recorded_by  TEXT,
    created_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS delivery_items (
    delivery_id TEXT NOT NULL REFERENCES deliveries(id),
    carton_id   TEXT NOT NULL REFERENCES cartons(id),
    PRIMARY KEY(delivery_id, carton_id)
);

-- 企业领用单（依据授权凭证，按获准规格领用）
CREATE TABLE IF NOT EXISTS issues (
    id             TEXT PRIMARY KEY,
    producer_ref   TEXT NOT NULL,
    attestation_id TEXT NOT NULL REFERENCES attestations(id),
    spec           TEXT NOT NULL,
    issued_at      TEXT NOT NULL,
    recorded_by    TEXT,
    created_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS issue_items (
    issue_id TEXT NOT NULL REFERENCES issues(id),
    code     TEXT NOT NULL REFERENCES codes(code),
    PRIMARY KEY(issue_id, code)
);

-- 实际生产批次
CREATE TABLE IF NOT EXISTS product_batches (
    id             TEXT PRIMARY KEY,
    producer_ref   TEXT NOT NULL,
    attestation_id TEXT NOT NULL REFERENCES attestations(id),
    product_ref    TEXT NOT NULL,
    spec           TEXT NOT NULL,
    produced_at    TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

-- 贴附记录：一个编号只能贴附一个产品批次。
-- client_event_id 为离线设备事件幂等键，重传同一事件不得产生第二条贴附。
CREATE TABLE IF NOT EXISTS attachments (
    code             TEXT PRIMARY KEY REFERENCES codes(code),
    product_batch_id TEXT NOT NULL REFERENCES product_batches(id),
    attestation_id   TEXT NOT NULL REFERENCES attestations(id),
    spec             TEXT NOT NULL,
    device_ref       TEXT,
    client_event_id  TEXT NOT NULL UNIQUE,
    occurred_at      TEXT NOT NULL,               -- 设备上报的实际贴附时间
    received_at      TEXT NOT NULL,               -- 服务端接收时间
    created_at       TEXT NOT NULL
);

-- 遗失报告
CREATE TABLE IF NOT EXISTS lost_reports (
    id             TEXT PRIMARY KEY,
    code           TEXT NOT NULL REFERENCES codes(code),
    reported_by    TEXT NOT NULL,
    custodian_type TEXT NOT NULL,                 -- 挂失时标签的最后保管方类型
    custodian_ref  TEXT NOT NULL,                 -- 挂失时标签的最后保管方
    note           TEXT,
    status         TEXT NOT NULL CHECK(status IN ('open', 'recovered', 'voided')),
    reported_at    TEXT NOT NULL,
    resolved_at    TEXT
);

-- 作废回收记录
CREATE TABLE IF NOT EXISTS voids (
    id             TEXT PRIMARY KEY,
    code           TEXT NOT NULL REFERENCES codes(code),
    reason         TEXT NOT NULL,                 -- damaged / recovered / recall / expired_unused / admin
    custodian_type TEXT,
    custodian_ref  TEXT,
    note           TEXT,
    voided_at      TEXT NOT NULL,
    recorded_by    TEXT
);

-- 每个编号的去向链（保管链事件，只追加）
CREATE TABLE IF NOT EXISTS custody_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    code           TEXT NOT NULL REFERENCES codes(code),
    event_type     TEXT NOT NULL,                 -- printed/packed/delivered/issued/attached/reported_lost/recovered/blocked/voided
    custodian_type TEXT NOT NULL,
    custodian_ref  TEXT NOT NULL,
    ref_type       TEXT,                          -- carton/delivery/issue/product_batch/lost_report/void
    ref_id         TEXT,
    note           TEXT,
    at             TEXT NOT NULL,
    recorded_by    TEXT
);
CREATE INDEX IF NOT EXISTS idx_custody_code ON custody_events(code, id);

-- 通用写接口幂等重放（离线设备重传的另一层保险是 attachments.client_event_id）
CREATE TABLE IF NOT EXISTS idempotent_requests (
    idempotency_key TEXT PRIMARY KEY,
    method          TEXT NOT NULL,
    path            TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status_code     INTEGER NOT NULL,
    response_body   BLOB NOT NULL,
    created_at      TEXT NOT NULL
);

-- 授权凭证不可修改、不可删除
DROP TRIGGER IF EXISTS trg_attestations_no_update;
CREATE TRIGGER trg_attestations_no_update
BEFORE UPDATE ON attestations
BEGIN
    SELECT RAISE(ABORT, 'attestations 为只追加外部凭证，禁止修改');
END;

DROP TRIGGER IF EXISTS trg_attestations_no_delete;
CREATE TRIGGER trg_attestations_no_delete
BEFORE DELETE ON attestations
BEGIN
    SELECT RAISE(ABORT, 'attestations 为只追加外部凭证，禁止删除');
END;

INSERT OR IGNORE INTO schema_migrations(version) VALUES ('002_label_lifecycle');

-- 标签码领用与核销：授权凭证为只读外部依据，标签状态机只进不退，事件台账只追加。

-- 授权凭证：企业现有授权材料的入库快照，仅允许新增，不允许修改或删除（见下方触发器）。
CREATE TABLE IF NOT EXISTS authorizations (
    credential_id     TEXT PRIMARY KEY,              -- 外部凭证编号
    producer_ref      TEXT NOT NULL,                 -- 生产企业来源编号
    producer_name     TEXT NOT NULL,                 -- 凭证记载的企业名称（验真展示快照）
    product_ref       TEXT NOT NULL,                 -- 获准使用标志的产品（规格）来源编号
    spec_name         TEXT NOT NULL,                 -- 产品规格名称
    batch_ref         TEXT,                          -- 凭证关联的备案/展品批次来源编号
    inspection_digest TEXT NOT NULL,                 -- 检测材料摘要
    valid_until       TEXT NOT NULL,                 -- 授权截止时间（ISO 8601 带偏移）
    issued_at         TEXT,                          -- 凭证签发时间
    content_digest    TEXT NOT NULL,                 -- 凭证内容摘要：重复导入据此判定是否一致
    received_at       TEXT NOT NULL                  -- 本服务收录时间
);

CREATE TRIGGER authorizations_no_update
BEFORE UPDATE ON authorizations
BEGIN
    SELECT RAISE(FAIL, '授权凭证是只读外部依据，禁止修改');
END;

CREATE TRIGGER authorizations_no_delete
BEFORE DELETE ON authorizations
BEGIN
    SELECT RAISE(FAIL, '授权凭证是只读外部依据，禁止删除');
END;

-- 印厂印制批次：按获准规格预制带验证编号的标签。
CREATE TABLE IF NOT EXISTS print_batches (
    batch_id     TEXT PRIMARY KEY,                    -- 印制批次号
    printer_ref  TEXT NOT NULL,                       -- 印厂来源编号
    printer_name TEXT,                                -- 印厂名称
    product_ref  TEXT NOT NULL,                       -- 本批标签预印对应的产品规格
    spec_name    TEXT NOT NULL,
    printed_at   TEXT NOT NULL,
    created_at   TEXT NOT NULL
);

-- 交付箱件：标签按箱密封、整箱交付给生产企业。
CREATE TABLE IF NOT EXISTS boxes (
    box_id                    TEXT PRIMARY KEY,
    print_batch_id            TEXT NOT NULL REFERENCES print_batches(batch_id),
    label_count               INTEGER NOT NULL,
    status                    TEXT NOT NULL CHECK (status IN ('packed', 'delivered')),
    delivered_to_producer_ref  TEXT,
    delivered_to_producer_name TEXT,
    packed_at                 TEXT NOT NULL,
    delivered_at              TEXT,
    created_at                TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_boxes_batch ON boxes(print_batch_id);

-- 单个验证编号及其当前状态。
-- printed 已印制 → packed 已装箱 → delivered 已交付企业 → issued 已领用 → applied 已贴附；
-- frozen（授权到期冻结等）与 void（作废）为异常终态。
-- lost_flag 是叠加在非终态上的挂失标记，可由 recovered 解除。
CREATE TABLE IF NOT EXISTS labels (
    code              TEXT PRIMARY KEY,              -- 验证编号
    print_batch_id    TEXT NOT NULL REFERENCES print_batches(batch_id),
    box_id            TEXT REFERENCES boxes(box_id),
    status            TEXT NOT NULL CHECK (status IN (
                        'printed', 'packed', 'delivered', 'issued', 'applied', 'frozen', 'void'
                      )),
    lost_flag         INTEGER NOT NULL DEFAULT 0,
    holder_type       TEXT,                          -- printer / producer / system
    holder_ref        TEXT,                          -- 当前保管人来源编号
    holder_name       TEXT,                          -- 当前保管人名称（溯源展示）
    credential_id     TEXT REFERENCES authorizations(credential_id),
    producer_ref      TEXT,                          -- 领用后快照
    producer_name     TEXT,                          -- 领用后快照（公众验真展示）
    product_ref       TEXT,
    spec_name         TEXT,
    product_batch_ref TEXT,                          -- 实际贴附的产品批次
    applied_at        TEXT,
    lost_at           TEXT,
    frozen_at         TEXT,
    voided_at         TEXT,
    void_reason       TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_labels_batch      ON labels(print_batch_id);
CREATE INDEX IF NOT EXISTS idx_labels_box        ON labels(box_id);
CREATE INDEX IF NOT EXISTS idx_labels_credential ON labels(credential_id);
CREATE INDEX IF NOT EXISTS idx_labels_status     ON labels(status);

-- 编号去向台账：只追加，记录每次状态/保管人变化；离线贴附靠 (device_id, device_event_id) 去重。
CREATE TABLE IF NOT EXISTS label_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    code              TEXT NOT NULL REFERENCES labels(code),
    event_type        TEXT NOT NULL CHECK (event_type IN (
                        'printed', 'packed', 'delivered', 'issued', 'applied',
                        'lost', 'recovered', 'frozen', 'voided'
                      )),
    actor_type        TEXT NOT NULL,                 -- printer / courier / producer / device / brand-admin / system
    actor_ref         TEXT NOT NULL,
    holder_type       TEXT,
    holder_ref        TEXT,
    holder_name       TEXT,
    device_id         TEXT,
    device_event_id   TEXT,
    product_batch_ref TEXT,
    event_time        TEXT NOT NULL,                 -- 业务发生时间（离线事件为设备时间）
    recorded_at       TEXT NOT NULL,                 -- 服务器收录时间
    payload           TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_label_events_code ON label_events(code, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_label_events_device_unique
    ON label_events(device_id, device_event_id)
    WHERE device_id IS NOT NULL AND device_event_id IS NOT NULL;

CREATE TRIGGER label_events_no_update
BEFORE UPDATE ON label_events
BEGIN
    SELECT RAISE(FAIL, '标签事件台账只允许追加，禁止修改');
END;

CREATE TRIGGER label_events_no_delete
BEFORE DELETE ON label_events
BEGIN
    SELECT RAISE(FAIL, '标签事件台账只允许追加，禁止删除');
END;

INSERT OR IGNORE INTO schema_migrations(version) VALUES ('002_label_lifecycle');

// Package domain 实现标签码领用与核销的领域服务：
// 授权凭证（只追加外部依据）→ 印制批次 → 箱件 → 交付 → 企业领用 → 产品贴附 → 挂失/停用/作废，
// 并提供公开验证与品牌方保管链追溯。
package domain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

// 哨兵错误，HTTP 层据此映射状态码。
var (
	ErrNotFound       = errors.New("记录不存在")
	ErrConflict       = errors.New("操作与标签当前状态冲突")
	ErrValidation     = errors.New("请求参数不合法")
	ErrAttestExpired  = errors.New("授权凭证不在有效期内")
	ErrAttestMismatch = errors.New("授权凭证与生产企业或获准规格不符")
)

// Service 依托单个 Store 组织全部用例。
type Service struct {
	store *store.Store
}

func New(s *store.Store) *Service { return &Service{store: s} }

func (s *Service) db() *sql.DB { return s.store.DB }

// nowText 返回归一化为 UTC 的 RFC3339 当前时间串。
func (s *Service) nowText() string { return s.store.Now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------------------
// 输入结构
// ---------------------------------------------------------------------------

type ImportAttestationInput struct {
	ID               string `json:"id,omitempty"` // 可空，服务端生成
	SourceRef        string `json:"source_ref"`   // 外部授权系统记录号
	ProducerRef      string `json:"producer_ref"`
	ProductRef       string `json:"product_ref"`
	BatchRef         string `json:"batch_ref"`
	Spec             string `json:"spec"` // 获准的产品规格
	InspectionDigest string `json:"inspection_digest"`
	ValidFrom        string `json:"valid_from,omitempty"` // RFC3339，可空
	ValidUntil       string `json:"valid_until"`          // RFC3339
}

type RegisterPrintBatchInput struct {
	ID         string   `json:"id,omitempty"`
	PrinterRef string   `json:"printer_ref"`
	Note       string   `json:"note,omitempty"`
	PrintedAt  string   `json:"printed_at"`
	Codes      []string `json:"codes"` // 印厂预制的验证编号
}

type PackCartonInput struct {
	ID           string   `json:"id,omitempty"`
	PrintBatchID string   `json:"print_batch_id"`
	Codes        []string `json:"codes"`
	Note         string   `json:"note,omitempty"`
}

type CreateDeliveryInput struct {
	ID          string   `json:"id,omitempty"`
	ProducerRef string   `json:"producer_ref"`
	ReceiverRef string   `json:"receiver_ref,omitempty"`
	CartonIDs   []string `json:"carton_ids"`
	DeliveredAt string   `json:"delivered_at"`
}

type IssueInput struct {
	ID            string   `json:"id,omitempty"`
	ProducerRef   string   `json:"producer_ref"`
	AttestationID string   `json:"attestation_id"` // 领用必须以授权凭证为依据
	Codes         []string `json:"codes"`
	IssuedAt      string   `json:"issued_at"`
}

type RegisterProductBatchInput struct {
	ID            string `json:"id,omitempty"`
	ProducerRef   string `json:"producer_ref"`
	AttestationID string `json:"attestation_id"`
	ProductRef    string `json:"product_ref"`
	ProducedAt    string `json:"produced_at"`
}

type AttachInput struct {
	Code           string `json:"code"`
	ProductBatchID string `json:"product_batch_id"`
	DeviceRef      string `json:"device_ref,omitempty"`
	ClientEventID  string `json:"client_event_id"` // 离线设备事件幂等键
	OccurredAt     string `json:"occurred_at"`     // 设备端实际贴附时间
}

type LostInput struct {
	Note       string `json:"note,omitempty"`
	ReportedAt string `json:"reported_at"`
}

type VoidInput struct {
	Reason   string `json:"reason"` // damaged / recovered / recall / expired_unused / admin
	Note     string `json:"note,omitempty"`
	VoidedAt string `json:"voided_at"`
}

// ---------------------------------------------------------------------------
// 输出结构
// ---------------------------------------------------------------------------

type Attestation struct {
	ID               string `json:"id"`
	SourceRef        string `json:"source_ref"`
	ProducerRef      string `json:"producer_ref"`
	ProductRef       string `json:"product_ref"`
	BatchRef         string `json:"batch_ref"`
	Spec             string `json:"spec"`
	InspectionDigest string `json:"inspection_digest"`
	ValidFrom        string `json:"valid_from,omitempty"`
	ValidUntil       string `json:"valid_until"`
	ImportedAt       string `json:"imported_at"`
}

type PrintBatch struct {
	ID         string   `json:"id"`
	PrinterRef string   `json:"printer_ref"`
	Note       string   `json:"note,omitempty"`
	PrintedAt  string   `json:"printed_at"`
	CodeCount  int      `json:"code_count"`
	Codes      []string `json:"codes,omitempty"`
}

type ProductBatch struct {
	ID            string `json:"id"`
	ProducerRef   string `json:"producer_ref"`
	AttestationID string `json:"attestation_id"`
	ProductRef    string `json:"product_ref"`
	Spec          string `json:"spec"`
	ProducedAt    string `json:"produced_at"`
}

type AttachResult struct {
	Code           string `json:"code"`
	ProductBatchID string `json:"product_batch_id"`
	Spec           string `json:"spec"`
	DeviceRef      string `json:"device_ref,omitempty"`
	ClientEventID  string `json:"client_event_id"`
	OccurredAt     string `json:"occurred_at"`
	ReceivedAt     string `json:"received_at"`
	Replayed       bool   `json:"replayed"` // 离线重传命中同一事件
}

type CustodyEvent struct {
	Sequence      int    `json:"sequence"`
	EventType     string `json:"event_type"`
	CustodianType string `json:"custodian_type"`
	CustodianRef  string `json:"custodian_ref"`
	RefType       string `json:"ref_type,omitempty"`
	RefID         string `json:"ref_id,omitempty"`
	Note          string `json:"note,omitempty"`
	At            string `json:"at"`
	RecordedBy    string `json:"recorded_by,omitempty"`
}

type Trace struct {
	Code                 string         `json:"code"`
	PrintBatchID         string         `json:"print_batch_id"`
	Serial               string         `json:"serial"`
	Status               string         `json:"status"`
	PriorStatus          string         `json:"prior_status,omitempty"`
	CartonID             string         `json:"carton_id,omitempty"`
	HolderProducerRef    string         `json:"holder_producer_ref,omitempty"`
	SpecLocked           string         `json:"spec_locked,omitempty"`
	CurrentCustodianType string         `json:"current_custodian_type"`
	CurrentCustodianRef  string         `json:"current_custodian_ref"`
	Attached             *AttachResult  `json:"attached,omitempty"`
	Events               []CustodyEvent `json:"events"`
}

type Verification struct {
	Code           string   `json:"code"`
	Valid          bool     `json:"valid"`
	Status         string   `json:"status"`
	Abnormal       bool     `json:"abnormal"`
	Reasons        []string `json:"reasons"`
	ProducerRef    string   `json:"producer_ref,omitempty"` // 只有已贴附产品才对外显示企业
	ProductRef     string   `json:"product_ref,omitempty"`
	ProductBatchID string   `json:"product_batch_id,omitempty"`
	Spec           string   `json:"spec,omitempty"`
	AttachedAt     string   `json:"attached_at,omitempty"`
	ValidUntil     string   `json:"valid_until,omitempty"`
	Message        string   `json:"message"`
}

type CustodianCount struct {
	CustodianType string `json:"custodian_type"`
	CustodianRef  string `json:"custodian_ref"`
	Count         int    `json:"count"`
}

type LostCustodianSummary struct {
	PrintBatchID    string           `json:"print_batch_id"`
	LostTotal       int              `json:"lost_total"`
	OpenLostTotal   int              `json:"open_lost_total"`
	ByLastCustodian []CustodianCount `json:"by_last_custodian"`
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func parseTime(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("%w: %s 不能为空", ErrValidation, field)
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s 不是带时区偏移的 ISO 8601 时间", ErrValidation, field)
	}
	// 统一归一化为 UTC，保证数据库内时间串的字典序与时间序一致。
	return t.UTC(), nil
}

func optionalTime(field, value string) (time.Time, bool, error) {
	if value == "" {
		return time.Time{}, false, nil
	}
	t, err := parseTime(field, value)
	return t, true, err
}

func requireNonEmpty(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s 不能为空", ErrValidation, field)
	}
	return nil
}

type attestationRow struct {
	id, producerRef, productRef, batchRef, spec, digest string
	validFrom                                           sql.NullTime
	validUntil                                          time.Time
}

func scanAttestation(q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ctx context.Context, id string) (attestationRow, error) {
	var a attestationRow
	var vf, vu string
	err := q.QueryRowContext(ctx,
		`SELECT id, producer_ref, product_ref, batch_ref, spec, inspection_digest,
		        COALESCE(valid_from,''), valid_until
		 FROM attestations WHERE id = ?`, id).
		Scan(&a.id, &a.producerRef, &a.productRef, &a.batchRef, &a.spec, &a.digest, &vf, &vu)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: 授权凭证 %s", ErrNotFound, id)
	}
	if err != nil {
		return a, err
	}
	if vf != "" {
		t, perr := time.Parse(time.RFC3339, vf)
		if perr != nil {
			return a, perr
		}
		a.validFrom = sql.NullTime{Time: t, Valid: true}
	}
	t, err := time.Parse(time.RFC3339, vu)
	if err != nil {
		return a, err
	}
	a.validUntil = t
	return a, nil
}

// attestationCoversAt 校验凭证在 at 时刻对 producer/spec 有效。
// 凭证是只追加的外部依据，系统只做比对，绝不改写它。
func attestationCoversAt(a attestationRow, producerRef, spec string, at time.Time) error {
	if producerRef != "" && producerRef != a.producerRef {
		return fmt.Errorf("%w: 凭证属于 %s", ErrAttestMismatch, a.producerRef)
	}
	if spec != "" && spec != a.spec {
		return fmt.Errorf("%w: 获准规格为 %s", ErrAttestMismatch, a.spec)
	}
	if a.validFrom.Valid && at.Before(a.validFrom.Time) {
		return fmt.Errorf("%w: 授权尚未生效", ErrAttestExpired)
	}
	if at.After(a.validUntil) {
		return fmt.Errorf("%w: 授权已于 %s 到期", ErrAttestExpired, a.validUntil.Format(time.RFC3339))
	}
	return nil
}

func insertCustody(ctx context.Context, tx *sql.Tx, code, eventType, custodianType, custodianRef, refType, refID, note, at, recordedBy string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO custody_events
		   (code, event_type, custodian_type, custodian_ref, ref_type, ref_id, note, at, recorded_by)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		code, eventType, custodianType, custodianRef, refType, refID, note, at, recordedBy)
	return err
}

// ---------------------------------------------------------------------------
// 用例：授权凭证导入
// ---------------------------------------------------------------------------

func (s *Service) ImportAttestation(ctx context.Context, in ImportAttestationInput, actor string) (*Attestation, error) {
	if err := requireNonEmpty("source_ref", in.SourceRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("producer_ref", in.ProducerRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("product_ref", in.ProductRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("batch_ref", in.BatchRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("spec", in.Spec); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("inspection_digest", in.InspectionDigest); err != nil {
		return nil, err
	}
	from, hasFrom, err := optionalTime("valid_from", in.ValidFrom)
	if err != nil {
		return nil, err
	}
	until, err := parseTime("valid_until", in.ValidUntil)
	if err != nil {
		return nil, err
	}
	if hasFrom && !from.Before(until) {
		return nil, fmt.Errorf("%w: valid_from 必须早于 valid_until", ErrValidation)
	}

	id := in.ID
	if id == "" {
		id = newID("att")
	}
	importedAt := s.nowText()

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 同一外部记录号重复导入 → 冲突（凭证只追加，也不允许用导入覆盖）。
	var dup string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM attestations WHERE producer_ref = ? AND source_ref = ?`,
		in.ProducerRef, in.SourceRef).Scan(&dup)
	switch {
	case err == nil:
		return nil, fmt.Errorf("%w: 外部凭证记录 %s 已导入（%s）", ErrConflict, in.SourceRef, dup)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	validFrom := ""
	if hasFrom {
		validFrom = from.Format(time.RFC3339)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO attestations
		   (id, source_ref, producer_ref, product_ref, batch_ref, spec, inspection_digest,
		    valid_from, valid_until, imported_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, in.SourceRef, in.ProducerRef, in.ProductRef, in.BatchRef, in.Spec,
		in.InspectionDigest, validFrom, until.Format(time.RFC3339), importedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Attestation{
		ID: id, SourceRef: in.SourceRef, ProducerRef: in.ProducerRef,
		ProductRef: in.ProductRef, BatchRef: in.BatchRef, Spec: in.Spec,
		InspectionDigest: in.InspectionDigest, ValidFrom: validFrom,
		ValidUntil: until.Format(time.RFC3339), ImportedAt: importedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// 用例：印制批次与预制编号
// ---------------------------------------------------------------------------

func (s *Service) RegisterPrintBatch(ctx context.Context, in RegisterPrintBatchInput, actor string) (*PrintBatch, error) {
	if err := requireNonEmpty("printer_ref", in.PrinterRef); err != nil {
		return nil, err
	}
	printedAt, err := parseTime("printed_at", in.PrintedAt)
	if err != nil {
		return nil, err
	}
	if len(in.Codes) == 0 {
		return nil, fmt.Errorf("%w: codes 至少包含一个预制编号", ErrValidation)
	}
	seen := map[string]bool{}
	for _, c := range in.Codes {
		if c == "" {
			return nil, fmt.Errorf("%w: codes 含空编号", ErrValidation)
		}
		if seen[c] {
			return nil, fmt.Errorf("%w: 编号 %s 重复", ErrValidation, c)
		}
		seen[c] = true
	}

	id := in.ID
	if id == "" {
		id = newID("pb")
	}
	now := s.nowText()
	at := printedAt.Format(time.RFC3339)

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO print_batches (id, printer_ref, note, printed_at, created_at) VALUES (?,?,?,?,?)`,
		id, in.PrinterRef, in.Note, at, now)
	if err != nil {
		return nil, err
	}
	for i, code := range in.Codes {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO codes (code, print_batch_id, serial, status, current_custodian_type,
			                    current_custodian_ref, created_at, updated_at)
			 VALUES (?,?,?, 'printed', 'printer', ?, ?, ?)`,
			code, id, fmt.Sprintf("%06d", i+1), in.PrinterRef, now, now)
		if err != nil {
			return nil, fmt.Errorf("%w: 编号 %s 已存在", ErrConflict, code)
		}
		if err := insertCustody(ctx, tx, code, "printed", "printer", in.PrinterRef,
			"print_batch", id, "", at, actor); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	codesCopy := append([]string(nil), in.Codes...)
	return &PrintBatch{ID: id, PrinterRef: in.PrinterRef, Note: in.Note,
		PrintedAt: at, CodeCount: len(codesCopy), Codes: codesCopy}, nil
}

// ---------------------------------------------------------------------------
// 用例：装箱
// ---------------------------------------------------------------------------

type Carton struct {
	ID           string   `json:"id"`
	PrintBatchID string   `json:"print_batch_id"`
	DeliveryID   string   `json:"delivery_id,omitempty"`
	CodeCount    int      `json:"code_count"`
	Codes        []string `json:"codes,omitempty"`
	Note         string   `json:"note,omitempty"`
}

func (s *Service) PackCarton(ctx context.Context, in PackCartonInput, actor string) (*Carton, error) {
	if err := requireNonEmpty("print_batch_id", in.PrintBatchID); err != nil {
		return nil, err
	}
	if len(in.Codes) == 0 {
		return nil, fmt.Errorf("%w: codes 不能为空", ErrValidation)
	}
	id := in.ID
	if id == "" {
		id = newID("ct")
	}
	now := s.nowText()

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var printerRef string
	err = tx.QueryRowContext(ctx,
		`SELECT printer_ref FROM print_batches WHERE id = ?`, in.PrintBatchID).Scan(&printerRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 印制批次 %s", ErrNotFound, in.PrintBatchID)
	}
	if err != nil {
		return nil, err
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO cartons (id, print_batch_id, note, created_at) VALUES (?,?,?,?)`,
		id, in.PrintBatchID, in.Note, now); err != nil {
		return nil, err
	}

	for _, code := range in.Codes {
		var status, batchID string
		err = tx.QueryRowContext(ctx,
			`SELECT status, print_batch_id FROM codes WHERE code = ?`, code).
			Scan(&status, &batchID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 编号 %s 未印制", ErrNotFound, code)
		}
		if err != nil {
			return nil, err
		}
		if batchID != in.PrintBatchID {
			return nil, fmt.Errorf("%w: 编号 %s 不属于该印制批次", ErrValidation, code)
		}
		if status != "printed" {
			return nil, fmt.Errorf("%w: 编号 %s 当前状态 %s，不能装箱", ErrConflict, code, status)
		}
	}
	for _, code := range in.Codes {
		if _, err = tx.ExecContext(ctx,
			`UPDATE codes SET status='packed', carton_id=?, updated_at=? WHERE code=?`,
			id, now, code); err != nil {
			return nil, err
		}
		if err := insertCustody(ctx, tx, code, "packed", "printer", printerRef,
			"carton", id, "", now, actor); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Carton{ID: id, PrintBatchID: in.PrintBatchID, CodeCount: len(in.Codes),
		Codes: append([]string(nil), in.Codes...), Note: in.Note}, nil
}

// ---------------------------------------------------------------------------
// 用例：箱件交付企业
// ---------------------------------------------------------------------------

type Delivery struct {
	ID          string   `json:"id"`
	ProducerRef string   `json:"producer_ref"`
	ReceiverRef string   `json:"receiver_ref,omitempty"`
	CartonIDs   []string `json:"carton_ids"`
	DeliveredAt string   `json:"delivered_at"`
}

func (s *Service) CreateDelivery(ctx context.Context, in CreateDeliveryInput, actor string) (*Delivery, error) {
	if err := requireNonEmpty("producer_ref", in.ProducerRef); err != nil {
		return nil, err
	}
	if len(in.CartonIDs) == 0 {
		return nil, fmt.Errorf("%w: carton_ids 不能为空", ErrValidation)
	}
	deliveredAt, err := parseTime("delivered_at", in.DeliveredAt)
	if err != nil {
		return nil, err
	}
	id := in.ID
	if id == "" {
		id = newID("dl")
	}
	at := deliveredAt.Format(time.RFC3339)
	now := s.nowText()

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO deliveries (id, producer_ref, receiver_ref, delivered_at, recorded_by, created_at)
		 VALUES (?,?,?,?,?,?)`,
		id, in.ProducerRef, in.ReceiverRef, at, actor, now); err != nil {
		return nil, err
	}

	custodianRef := in.ProducerRef
	if in.ReceiverRef != "" {
		custodianRef = in.ReceiverRef // 交付给企业指定的收货方时，保管方记收货方
	}
	for _, cartonID := range in.CartonIDs {
		var exists int
		var already string
		err = tx.QueryRowContext(ctx,
			`SELECT 1, COALESCE(delivery_id,'') FROM cartons WHERE id = ?`,
			cartonID).Scan(&exists, &already)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 箱件 %s", ErrNotFound, cartonID)
		}
		if err != nil {
			return nil, err
		}
		if already != "" {
			return nil, fmt.Errorf("%w: 箱件 %s 已交付（%s）", ErrConflict, cartonID, already)
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO delivery_items (delivery_id, carton_id) VALUES (?,?)`, id, cartonID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx,
			`UPDATE cartons SET delivery_id=? WHERE id=?`, id, cartonID); err != nil {
			return nil, err
		}

		rows, err := tx.QueryContext(ctx, `SELECT code FROM codes WHERE carton_id = ?`, cartonID)
		if err != nil {
			return nil, err
		}
		var codes []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return nil, err
			}
			codes = append(codes, c)
		}
		rows.Close()

		for _, code := range codes {
			var status string
			if err = tx.QueryRowContext(ctx, `SELECT status FROM codes WHERE code=?`, code).Scan(&status); err != nil {
				return nil, err
			}
			if status != "packed" {
				return nil, fmt.Errorf("%w: 编号 %s 状态 %s，箱件不能交付", ErrConflict, code, status)
			}
			if _, err = tx.ExecContext(ctx,
				`UPDATE codes SET status='delivered', holder_producer_ref=?,
				                  current_custodian_type='producer', current_custodian_ref=?, updated_at=?
				 WHERE code=?`,
				in.ProducerRef, custodianRef, now, code); err != nil {
				return nil, err
			}
			if err := insertCustody(ctx, tx, code, "delivered", "producer", custodianRef,
				"delivery", id, "", at, actor); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Delivery{ID: id, ProducerRef: in.ProducerRef, ReceiverRef: in.ReceiverRef,
		CartonIDs: append([]string(nil), in.CartonIDs...), DeliveredAt: at}, nil
}

// ---------------------------------------------------------------------------
// 用例：企业按授权凭证领用
// ---------------------------------------------------------------------------

type Issue struct {
	ID            string   `json:"id"`
	ProducerRef   string   `json:"producer_ref"`
	AttestationID string   `json:"attestation_id"`
	Spec          string   `json:"spec"`
	Codes         []string `json:"codes"`
	IssuedAt      string   `json:"issued_at"`
}

func (s *Service) IssueCodes(ctx context.Context, in IssueInput, actor string) (*Issue, error) {
	if err := requireNonEmpty("producer_ref", in.ProducerRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("attestation_id", in.AttestationID); err != nil {
		return nil, err
	}
	if len(in.Codes) == 0 {
		return nil, fmt.Errorf("%w: codes 不能为空", ErrValidation)
	}
	issuedAt, err := parseTime("issued_at", in.IssuedAt)
	if err != nil {
		return nil, err
	}
	id := in.ID
	if id == "" {
		id = newID("is")
	}
	at := issuedAt.Format(time.RFC3339)
	now := s.nowText()

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	att, err := scanAttestation(tx, ctx, in.AttestationID)
	if err != nil {
		return nil, err
	}
	// 凭证必须属于该领用企业；有效期在下方逐个核码之后再判定，
	// 这样停用/挂失/作废等状态冲突会优先、明确地返回。
	if att.producerRef != in.ProducerRef {
		return nil, fmt.Errorf("%w: 凭证属于 %s", ErrAttestMismatch, att.producerRef)
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO issues (id, producer_ref, attestation_id, spec, issued_at, recorded_by, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		id, in.ProducerRef, att.id, att.spec, at, actor, now); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for _, code := range in.Codes {
		if seen[code] {
			return nil, fmt.Errorf("%w: 编号 %s 在本单重复", ErrValidation, code)
		}
		seen[code] = true
		var status, holder string
		err = tx.QueryRowContext(ctx,
			`SELECT status, COALESCE(holder_producer_ref,'') FROM codes WHERE code=?`, code).
			Scan(&status, &holder)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 编号 %s 不存在", ErrNotFound, code)
		}
		if err != nil {
			return nil, err
		}
		switch {
		case status == "blocked":
			return nil, fmt.Errorf("%w: 编号 %s 已因授权失效停用，禁止领用", ErrConflict, code)
		case status == "lost":
			return nil, fmt.Errorf("%w: 编号 %s 已挂失，禁止领用", ErrConflict, code)
		case status == "void":
			return nil, fmt.Errorf("%w: 编号 %s 已作废", ErrConflict, code)
		case status != "delivered":
			return nil, fmt.Errorf("%w: 编号 %s 当前状态 %s，不能领用", ErrConflict, code, status)
		}
		if holder != in.ProducerRef {
			return nil, fmt.Errorf("%w: 编号 %s 未交付给该企业", ErrAttestMismatch, code)
		}
	}

	// 所有编号状态合法后，领用时凭证必须仍在有效期——过期授权下未领用标签无法流出。
	if err := attestationCoversAt(att, in.ProducerRef, "", issuedAt); err != nil {
		return nil, err
	}

	for _, code := range in.Codes {
		if _, err = tx.ExecContext(ctx,
			`UPDATE codes SET status='issued', issue_id=?, spec_locked=?,
			                  current_custodian_type='producer', current_custodian_ref=?, updated_at=?
			 WHERE code=?`,
			id, att.spec, in.ProducerRef, now, code); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO issue_items (issue_id, code) VALUES (?,?)`, id, code); err != nil {
			return nil, err
		}
		if err := insertCustody(ctx, tx, code, "issued", "producer", in.ProducerRef,
			"issue", id, "规格:"+att.spec, at, actor); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Issue{ID: id, ProducerRef: in.ProducerRef, AttestationID: att.id,
		Spec: att.spec, Codes: append([]string(nil), in.Codes...), IssuedAt: at}, nil
}

// ---------------------------------------------------------------------------
// 用例：产品批次登记
// ---------------------------------------------------------------------------

func (s *Service) RegisterProductBatch(ctx context.Context, in RegisterProductBatchInput, actor string) (*ProductBatch, error) {
	if err := requireNonEmpty("producer_ref", in.ProducerRef); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("product_ref", in.ProductRef); err != nil {
		return nil, err
	}
	producedAt, err := parseTime("produced_at", in.ProducedAt)
	if err != nil {
		return nil, err
	}
	id := in.ID
	if id == "" {
		id = newID("pbat")
	}

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	att, err := scanAttestation(tx, ctx, in.AttestationID)
	if err != nil {
		return nil, err
	}
	if err := attestationCoversAt(att, in.ProducerRef, "", producedAt); err != nil {
		return nil, err
	}
	if in.ProductRef != att.productRef {
		return nil, fmt.Errorf("%w: 凭证获准产品为 %s", ErrAttestMismatch, att.productRef)
	}

	now := s.nowText()
	at := producedAt.Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO product_batches (id, producer_ref, attestation_id, product_ref, spec, produced_at, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		id, in.ProducerRef, att.id, att.productRef, att.spec, at, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ProductBatch{ID: id, ProducerRef: in.ProducerRef, AttestationID: att.id,
		ProductRef: att.productRef, Spec: att.spec, ProducedAt: at}, nil
}

// ---------------------------------------------------------------------------
// 用例：离线设备贴附（幂等；一个编号只能对应一个产品批次）
// ---------------------------------------------------------------------------

func (s *Service) AttachCode(ctx context.Context, in AttachInput, actor string) (*AttachResult, error) {
	if err := requireNonEmpty("code", in.Code); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("product_batch_id", in.ProductBatchID); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("client_event_id", in.ClientEventID); err != nil {
		return nil, fmt.Errorf("%w: client_event_id 是离线重传幂等所必需的", ErrValidation)
	}
	occurredAt, err := parseTime("occurred_at", in.OccurredAt)
	if err != nil {
		return nil, err
	}

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 1) 事件重放：同一 client_event_id 直接回放在先结果，绝不产生第二条贴附。
	var existingCode, existingBatch, existingSpec, existingDevice, existingOccurred, existingReceived string
	err = tx.QueryRowContext(ctx,
		`SELECT code, product_batch_id, spec, COALESCE(device_ref,''), occurred_at, received_at
		 FROM attachments WHERE client_event_id = ?`, in.ClientEventID).
		Scan(&existingCode, &existingBatch, &existingSpec, &existingDevice, &existingOccurred, &existingReceived)
	switch {
	case err == nil:
		if existingCode != in.Code || existingBatch != in.ProductBatchID {
			return nil, fmt.Errorf("%w: 幂等键 %s 已用于编号 %s/批次 %s，拒绝挪用到其它编号或批次",
				ErrConflict, in.ClientEventID, existingCode, existingBatch)
		}
		return &AttachResult{Code: existingCode, ProductBatchID: existingBatch, Spec: existingSpec,
			DeviceRef: existingDevice, ClientEventID: in.ClientEventID,
			OccurredAt: existingOccurred, ReceivedAt: existingReceived, Replayed: true}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	// 2) 产品批次与凭证；贴附业务时间以设备上报的 occurred_at 为准（离线晚到也按实际发生时判定）。
	var pbProducer, pbAttestation string
	err = tx.QueryRowContext(ctx,
		`SELECT producer_ref, attestation_id FROM product_batches WHERE id=?`, in.ProductBatchID).
		Scan(&pbProducer, &pbAttestation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 产品批次 %s", ErrNotFound, in.ProductBatchID)
	}
	if err != nil {
		return nil, err
	}
	att, err := scanAttestation(tx, ctx, pbAttestation)
	if err != nil {
		return nil, err
	}
	if err := attestationCoversAt(att, pbProducer, att.spec, occurredAt); err != nil {
		return nil, err
	}

	// 3) 编号状态：必须已由该企业领用且规格锁定一致。
	var status, holder, specLocked string
	err = tx.QueryRowContext(ctx,
		`SELECT status, COALESCE(holder_producer_ref,''), COALESCE(spec_locked,'')
		 FROM codes WHERE code=?`, in.Code).Scan(&status, &holder, &specLocked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 编号 %s 不存在", ErrNotFound, in.Code)
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "attached":
		var otherBatch string
		_ = tx.QueryRowContext(ctx, `SELECT product_batch_id FROM attachments WHERE code=?`, in.Code).Scan(&otherBatch)
		if otherBatch == in.ProductBatchID {
			// 同编号同批次但换了事件键：视为幂等成功但不新增记录。
			return nil, fmt.Errorf("%w: 编号 %s 已贴附该批次，请使用原始 client_event_id 重传",
				ErrConflict, in.Code)
		}
		return nil, fmt.Errorf("%w: 编号 %s 已贴附产品批次 %s，一个编号不能贴到两个不同批次",
			ErrConflict, in.Code, otherBatch)
	case "lost":
		return nil, fmt.Errorf("%w: 编号 %s 已挂失，不能贴附", ErrConflict, in.Code)
	case "blocked", "void":
		return nil, fmt.Errorf("%w: 编号 %s 已%s，不能贴附", ErrConflict, in.Code,
			map[string]string{"blocked": "停用", "void": "作废"}[status])
	case "issued":
	default:
		return nil, fmt.Errorf("%w: 编号 %s 状态 %s，尚未领用，不能贴附", ErrConflict, in.Code, status)
	}
	if holder != pbProducer {
		return nil, fmt.Errorf("%w: 编号 %s 非该企业领用", ErrAttestMismatch, in.Code)
	}
	if specLocked != "" && specLocked != att.spec {
		return nil, fmt.Errorf("%w: 编号锁定规格 %s，与批次规格 %s 不符",
			ErrAttestMismatch, specLocked, att.spec)
	}

	at := occurredAt.Format(time.RFC3339)
	now := s.nowText()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO attachments
		   (code, product_batch_id, attestation_id, spec, device_ref, client_event_id, occurred_at, received_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		in.Code, in.ProductBatchID, att.id, att.spec, in.DeviceRef, in.ClientEventID, at, now, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE codes SET status='attached', attached_batch_id=?, current_custodian_type='on_product',
		                  current_custodian_ref=?, updated_at=? WHERE code=?`,
		in.ProductBatchID, in.ProductBatchID, now, in.Code); err != nil {
		return nil, err
	}
	if err := insertCustody(ctx, tx, in.Code, "attached", "on_product", in.ProductBatchID,
		"product_batch", in.ProductBatchID, "设备:"+in.DeviceRef, at, actor); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &AttachResult{Code: in.Code, ProductBatchID: in.ProductBatchID, Spec: att.spec,
		DeviceRef: in.DeviceRef, ClientEventID: in.ClientEventID,
		OccurredAt: at, ReceivedAt: now, Replayed: false}, nil
}

// ---------------------------------------------------------------------------
// 用例：挂失 / 找回 / 作废
// ---------------------------------------------------------------------------

func (s *Service) ReportLost(ctx context.Context, code string, in LostInput, actor string) error {
	if err := requireNonEmpty("code", code); err != nil {
		return err
	}
	reportedAt, err := parseTime("reported_at", in.ReportedAt)
	if err != nil {
		return err
	}
	at := reportedAt.Format(time.RFC3339)
	now := s.nowText()

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status, custType, custRef string
	err = tx.QueryRowContext(ctx,
		`SELECT status, current_custodian_type, current_custodian_ref FROM codes WHERE code=?`, code).
		Scan(&status, &custType, &custRef)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 编号 %s", ErrNotFound, code)
	}
	if err != nil {
		return err
	}
	if status == "lost" {
		return fmt.Errorf("%w: 编号 %s 已在挂失状态", ErrConflict, code)
	}
	if status == "void" || status == "blocked" {
		return fmt.Errorf("%w: 编号 %s 已%s，无需挂失", ErrConflict, code,
			map[string]string{"void": "作废", "blocked": "停用"}[status])
	}

	rid := newID("lost")
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO lost_reports (id, code, reported_by, custodian_type, custodian_ref, note, status, reported_at)
		 VALUES (?,?,?,?,?,?,'open',?)`,
		rid, code, actor, custType, custRef, in.Note, at); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE codes SET status='lost', prior_status=?, current_custodian_type='unknown',
		                  current_custodian_ref='unknown', updated_at=? WHERE code=?`,
		status, now, code); err != nil {
		return err
	}
	note := in.Note
	if note == "" {
		note = "挂失前最后保管方:" + custType + "/" + custRef
	}
	if err := insertCustody(ctx, tx, code, "reported_lost", "unknown", "unknown",
		"lost_report", rid, note, at, actor); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverLost 找回标签，恢复挂失前状态与保管方。
func (s *Service) RecoverLost(ctx context.Context, code string, at string, actor string) error {
	recoveredAt, err := parseTime("at", at)
	if err != nil {
		return err
	}
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var priorStatus, holder string
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(prior_status,''), COALESCE(holder_producer_ref,'') FROM codes WHERE code=?`, code).
		Scan(&priorStatus, &holder)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 编号 %s", ErrNotFound, code)
	}
	if err != nil {
		return err
	}
	var status string
	_ = tx.QueryRowContext(ctx, `SELECT status FROM codes WHERE code=?`, code).Scan(&status)
	if status != "lost" || priorStatus == "" {
		return fmt.Errorf("%w: 编号 %s 未处于挂失状态", ErrConflict, code)
	}

	custType, custRef := "printer", ""
	switch priorStatus {
	case "printed", "packed":
		var printer string
		if err = tx.QueryRowContext(ctx,
			`SELECT pb.printer_ref FROM codes c JOIN print_batches pb ON pb.id=c.print_batch_id
			 WHERE c.code=?`, code).Scan(&printer); err != nil {
			return err
		}
		custType, custRef = "printer", printer
	case "delivered", "issued":
		custType, custRef = "producer", holder
	case "attached":
		var batch string
		if err = tx.QueryRowContext(ctx,
			`SELECT COALESCE(attached_batch_id,'') FROM codes WHERE code=?`, code).Scan(&batch); err != nil {
			return err
		}
		custType, custRef = "on_product", batch
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE codes SET status=?, prior_status=NULL, current_custodian_type=?,
		                  current_custodian_ref=?, updated_at=? WHERE code=?`,
		priorStatus, custType, custRef, s.nowText(), code); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE lost_reports SET status='recovered', resolved_at=?
		 WHERE code=? AND status='open'`,
		recoveredAt.Format(time.RFC3339), code); err != nil {
		return err
	}
	if err := insertCustody(ctx, tx, code, "recovered", custType, custRef,
		"", "", "标签找回", recoveredAt.Format(time.RFC3339), actor); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) VoidCode(ctx context.Context, code string, in VoidInput, actor string) error {
	if err := requireNonEmpty("reason", in.Reason); err != nil {
		return fmt.Errorf("%w: reason 不能为空", ErrValidation)
	}
	voidedAt, err := parseTime("voided_at", in.VoidedAt)
	if err != nil {
		return err
	}
	at := voidedAt.Format(time.RFC3339)

	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status, custType, custRef string
	err = tx.QueryRowContext(ctx,
		`SELECT status, current_custodian_type, current_custodian_ref FROM codes WHERE code=?`, code).
		Scan(&status, &custType, &custRef)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 编号 %s", ErrNotFound, code)
	}
	if err != nil {
		return err
	}
	if status == "void" {
		return fmt.Errorf("%w: 编号 %s 已作废", ErrConflict, code)
	}

	rid := newID("void")
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO voids (id, code, reason, custodian_type, custodian_ref, note, voided_at, recorded_by)
		 VALUES (?,?,?,?,?,?,?,?)`,
		rid, code, in.Reason, custType, custRef, in.Note, at, actor); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE codes SET status='void', current_custodian_type='brand',
		                  current_custodian_ref=?, updated_at=? WHERE code=?`,
		actor, s.nowText(), code); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE lost_reports SET status='voided', resolved_at=? WHERE code=? AND status='open'`,
		at, code); err != nil {
		return err
	}
	if err := insertCustody(ctx, tx, code, "voided", "brand", actor,
		"void", rid, in.Reason, at, actor); err != nil {
		return err
	}
	return tx.Commit()
}

// BlockExpiredUnused 停用授权已全部到期、但企业尚未领用的剩余标签。
// 规则：编号处于 delivered（已交付未领用），其持有企业当前不存在任何覆盖 now 的有效授权。
// 已贴附产品（attached）的历史合规板材绝不停用；issued 表示企业已领走，由企业回收流程处理，
// 也不在此批量停用范围——未领用的剩余标签必须停用，领用之后的去向由挂失/作废链覆盖。
func (s *Service) BlockExpiredUnused(ctx context.Context, now time.Time, actor string) (int, error) {
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	at := now.UTC().Format(time.RFC3339)
	rows, err := tx.QueryContext(ctx,
		`SELECT c.code, c.holder_producer_ref
		 FROM codes c
		 WHERE c.status = 'delivered'
		   AND NOT EXISTS (
		         SELECT 1 FROM attestations a
		         WHERE a.producer_ref = c.holder_producer_ref
		           AND COALESCE(a.valid_from, '') <= ?
		           AND a.valid_until >= ?
		       )`, at, at)
	if err != nil {
		return 0, err
	}
	type target struct{ code, holder string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.code, &t.holder); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, t)
	}
	rows.Close()

	for _, t := range targets {
		if _, err = tx.ExecContext(ctx,
			`UPDATE codes SET status='blocked', prior_status='delivered', updated_at=? WHERE code=?`,
			at, t.code); err != nil {
			return 0, err
		}
		if err := insertCustody(ctx, tx, t.code, "blocked", "producer", t.holder,
			"", "", "授权已全部到期，未领用标签停用", at, actor); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(targets), nil
}

// ---------------------------------------------------------------------------
// 用例：采购商公开验证
// ---------------------------------------------------------------------------

func (s *Service) Verify(ctx context.Context, code string) (*Verification, error) {
	v := &Verification{Code: code, Reasons: []string{}}

	var status, holder, batchID, printBatch string
	err := s.db().QueryRowContext(ctx,
		`SELECT status, COALESCE(holder_producer_ref,''), COALESCE(attached_batch_id,''), print_batch_id
		 FROM codes WHERE code=?`, code).Scan(&status, &holder, &batchID, &printBatch)
	if errors.Is(err, sql.ErrNoRows) {
		v.Status = "unknown"
		v.Valid = false
		v.Abnormal = true
		v.Message = "该编号不存在，谨防假冒"
		v.Reasons = append(v.Reasons, "code_not_found")
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	v.Status = status

	switch status {
	case "printed", "packed":
		v.Abnormal = true
		v.Message = "标签尚在印厂环节，未对应任何在售产品"
		v.Reasons = append(v.Reasons, "not_in_use")
		return v, nil
	case "delivered", "issued":
		// 未贴附产品时不对外显示企业名称，避免挪用者借扫码页自证。
		v.Abnormal = true
		v.Message = "标签已流出但尚未贴附产品，若板材在售则可能系挪用或遗失标签"
		v.Reasons = append(v.Reasons, "not_attached")
		return v, nil
	case "lost":
		v.Abnormal = true
		v.Message = "该标签已被报告遗失，扫码所见产品请视为可疑"
		v.Reasons = append(v.Reasons, "lost_reported")
		return v, nil
	case "blocked":
		v.Abnormal = true
		v.Message = "该标签因授权失效已被停用，不得使用"
		v.Reasons = append(v.Reasons, "blocked_expired_unused")
		return v, nil
	case "void":
		v.Abnormal = true
		var reason string
		_ = s.db().QueryRowContext(ctx,
			`SELECT reason FROM voids WHERE code=? ORDER BY voided_at DESC LIMIT 1`, code).Scan(&reason)
		v.Message = "该标签已作废回收"
		v.Reasons = append(v.Reasons, "void_"+reason)
		return v, nil
	case "attached":
		// 已贴附：展示当前对应产品规格。授权到期不追溯既往——
		// 只要贴附发生在凭证有效期内，已售出板材仍判合规。
		var pbProducer, pbProduct, spec, attID, occurredAt, validUntil string
		err = s.db().QueryRowContext(ctx,
			`SELECT pb.producer_ref, pb.product_ref, a.spec, a.attestation_id, a.occurred_at, att.valid_until
			 FROM attachments a
			 JOIN product_batches pb ON pb.id = a.product_batch_id
			 JOIN attestations att ON att.id = a.attestation_id
			 WHERE a.code=?`, code).
			Scan(&pbProducer, &pbProduct, &spec, &attID, &occurredAt, &validUntil)
		if err != nil {
			return nil, err
		}
		v.ProducerRef = pbProducer
		v.ProductRef = pbProduct
		v.ProductBatchID = batchID
		v.Spec = spec
		v.AttachedAt = occurredAt
		v.ValidUntil = validUntil

		ot, _ := time.Parse(time.RFC3339, occurredAt)
		ut, _ := time.Parse(time.RFC3339, validUntil)
		switch {
		case !ot.IsZero() && !ut.IsZero() && ot.After(ut):
			v.Abnormal = true
			v.Valid = false
			v.Message = "贴附时间晚于授权有效期，产品不具备授权"
			v.Reasons = append(v.Reasons, "attached_after_expiry")
		default:
			v.Valid = true
			v.Message = "验真通过：编号、产品规格与授权记录一致"
			if !ut.IsZero() && s.store.Now().After(ut) {
				// 告知采购商授权现已到期，但贴附在售效期内，不影响这批已售出板材的合规性。
				v.Reasons = append(v.Reasons, "attestation_expired_after_attach")
			}
		}
		return v, nil
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 用例：品牌方追溯
// ---------------------------------------------------------------------------

func (s *Service) Trace(ctx context.Context, code string) (*Trace, error) {
	t := &Trace{Code: code, Events: []CustodyEvent{}}
	err := s.db().QueryRowContext(ctx,
		`SELECT print_batch_id, serial, status, COALESCE(prior_status,''), COALESCE(carton_id,''),
		        COALESCE(holder_producer_ref,''), COALESCE(spec_locked,''),
		        current_custodian_type, current_custodian_ref
		 FROM codes WHERE code=?`, code).
		Scan(&t.PrintBatchID, &t.Serial, &t.Status, &t.PriorStatus, &t.CartonID,
			&t.HolderProducerRef, &t.SpecLocked, &t.CurrentCustodianType, &t.CurrentCustodianRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 编号 %s", ErrNotFound, code)
	}
	if err != nil {
		return nil, err
	}

	if t.Status == "attached" {
		var a AttachResult
		var device sql.NullString
		err = s.db().QueryRowContext(ctx,
			`SELECT product_batch_id, spec, device_ref, client_event_id, occurred_at, received_at
			 FROM attachments WHERE code=?`, code).
			Scan(&a.ProductBatchID, &a.Spec, &device, &a.ClientEventID, &a.OccurredAt, &a.ReceivedAt)
		if err != nil {
			return nil, err
		}
		a.Code = code
		a.DeviceRef = device.String
		t.Attached = &a
	}

	rows, err := s.db().QueryContext(ctx,
		`SELECT id, event_type, custodian_type, custodian_ref, COALESCE(ref_type,''),
		        COALESCE(ref_id,''), COALESCE(note,''), at, COALESCE(recorded_by,'')
		 FROM custody_events WHERE code=? ORDER BY id`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e CustodyEvent
		if err := rows.Scan(&e.Sequence, &e.EventType, &e.CustodianType, &e.CustodianRef,
			&e.RefType, &e.RefID, &e.Note, &e.At, &e.RecordedBy); err != nil {
			return nil, err
		}
		t.Events = append(t.Events, e)
	}
	return t, nil
}

// LostCustodians 汇总某印制批次下全部遗失标签的最后保管方，回答“整批遗失标签最后由谁保管”。
func (s *Service) LostCustodians(ctx context.Context, printBatchID string) (*LostCustodianSummary, error) {
	var exists int
	err := s.db().QueryRowContext(ctx, `SELECT 1 FROM print_batches WHERE id=?`, printBatchID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 印制批次 %s", ErrNotFound, printBatchID)
	}
	if err != nil {
		return nil, err
	}

	summary := &LostCustodianSummary{PrintBatchID: printBatchID, ByLastCustodian: []CustodianCount{}}
	rows, err := s.db().QueryContext(ctx,
		`SELECT lr.custodian_type, lr.custodian_ref, COUNT(*),
		        SUM(CASE WHEN lr.status='open' THEN 1 ELSE 0 END)
		 FROM lost_reports lr
		 JOIN codes c ON c.code = lr.code
		 WHERE c.print_batch_id = ?
		 GROUP BY lr.custodian_type, lr.custodian_ref
		 ORDER BY COUNT(*) DESC, lr.custodian_ref`, printBatchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cc CustodianCount
		var open int
		if err := rows.Scan(&cc.CustodianType, &cc.CustodianRef, &cc.Count, &open); err != nil {
			return nil, err
		}
		summary.LostTotal += cc.Count
		summary.OpenLostTotal += open
		summary.ByLastCustodian = append(summary.ByLastCustodian, cc)
	}
	return summary, nil
}

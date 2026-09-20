// Package store 实现标签码领用与核销的数据访问：以只读授权凭证为外部依据，
// 标签状态机只进不退，全部去向写入只追加事件台账。
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/vancemichael/092002-forest-brand-attest"
	_ "modernc.org/sqlite"
)

// 领域错误，HTTP 层据此映射状态码。
var (
	// ErrInvalid 输入不满足字段或状态机前置条件。
	ErrInvalid = errors.New("无效请求")
	// ErrNotFound 编号或单据不存在。
	ErrNotFound = errors.New("记录不存在")
	// ErrConflict 与既有状态冲突（含重复编号、重传换批次、终态后操作）。
	ErrConflict = errors.New("状态冲突")
)

// Store 是标签领域数据的唯一访问入口。单连接串行化写入，避免离线重传下的竞态。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open 打开数据库并应用内嵌迁移。
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(app.Migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		raw, err := fs.ReadFile(app.Migrations, "migrations/"+name)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, string(raw)); err != nil {
			return fmt.Errorf("应用迁移 %s: %w", name, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 输入/输出结构
// ---------------------------------------------------------------------------

// AuthorizationInput 是外部授权凭证的入库内容。凭证只能新增，不能修改。
type AuthorizationInput struct {
	CredentialID     string `json:"credential_id"`
	ProducerRef      string `json:"producer_ref"`
	ProducerName     string `json:"producer_name"`
	ProductRef       string `json:"product_ref"`
	SpecName         string `json:"spec_name"`
	BatchRef         string `json:"batch_ref"`
	InspectionDigest string `json:"inspection_digest"`
	ValidUntil       string `json:"valid_until"`
	IssuedAt         string `json:"issued_at"`
	ContentDigest    string `json:"content_digest"`
}

// PrintBatchInput 预制一批带验证编号的板材标签。
type PrintBatchInput struct {
	BatchID     string   `json:"batch_id"`
	PrinterRef  string   `json:"printer_ref"`
	PrinterName string   `json:"printer_name"`
	ProductRef  string   `json:"product_ref"`
	SpecName    string   `json:"spec_name"`
	PrintedAt   string   `json:"printed_at"`
	Codes       []string `json:"codes"`
}

// BoxPackInput 按印厂交付箱件封装标签。
type BoxPackInput struct {
	BoxID        string   `json:"box_id"`
	PrintBatchID string   `json:"print_batch_id"`
	Codes        []string `json:"codes"`
	PackedAt     string   `json:"packed_at"`
}

// BoxDeliverInput 整箱交付到生产企业。
type BoxDeliverInput struct {
	BoxID                   string `json:"box_id"`
	DeliveredToProducerRef  string `json:"delivered_to_producer_ref"`
	DeliveredToProducerName string `json:"delivered_to_producer_name"`
	// CourierRef 为交付承运人来源编号，记为交付事件的行为人。
	CourierRef  string `json:"courier_ref"`
	DeliveredAt string `json:"delivered_at"`
}

// IssueInput 企业按获准规格领用标签。
type IssueInput struct {
	CredentialID string   `json:"credential_id"`
	Codes        []string `json:"codes"`
	IssuedAt     string   `json:"issued_at"`
}

// ApplyItem 是一条离线贴附记录：编号贴到了哪个产品批次。
type ApplyItem struct {
	Code            string `json:"code"`
	ProductBatchRef string `json:"product_batch_ref"`
}

// ApplyInput 是离线设备回传的一批贴附事件。
// (DeviceID, DeviceEventID) 构成重传幂等键，重复回传不得产生第二条贴附记录。
type ApplyInput struct {
	DeviceID      string      `json:"device_id"`
	DeviceEventID string      `json:"device_event_id"`
	EventTime     string      `json:"event_time"`
	Items         []ApplyItem `json:"items"`
}

// ReportInput 标签遗失/寻回上报。
type ReportInput struct {
	Codes []string `json:"codes"`
	// ActorRef/ActorName 为上报并当前保管标签的一方（印厂或生产企业）。
	ActorType string `json:"actor_type"`
	ActorRef  string `json:"actor_ref"`
	ActorName string `json:"actor_name"`
	EventTime string `json:"event_time"`
	Note      string `json:"note"`
}

// VoidInput 品牌方作废未使用标签（含回收核销）。
type VoidInput struct {
	Codes  []string `json:"codes"`
	Reason string   `json:"reason"`
	// Recovered 表示实物已回收，事件原因记为回收核销。
	Recovered bool   `json:"recovered"`
	ActorRef  string `json:"actor_ref"`
	EventTime string `json:"event_time"`
}

// Result 描述一次写操作实际处理的编号（幂等重放时已处理编号不再重复计入）。
type Result struct {
	Affected []string `json:"affected"`
	Skipped  []string `json:"skipped"`
}

// MarshalJSON 保证空切片序列化为 [] 而非 null。
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result
	a := alias(r)
	if a.Affected == nil {
		a.Affected = []string{}
	}
	if a.Skipped == nil {
		a.Skipped = []string{}
	}
	return json.Marshal(a)
}

// ---------------------------------------------------------------------------
// 授权凭证（只读外部依据）
// ---------------------------------------------------------------------------

// ImportAuthorization 收录一张授权凭证。同一 credential_id 仅允许以相同内容摘要重复导入。
func (s *Store) ImportAuthorization(ctx context.Context, in AuthorizationInput) (created bool, err error) {
	in.CredentialID = strings.TrimSpace(in.CredentialID)
	required := map[string]string{
		"credential_id":     in.CredentialID,
		"producer_ref":      in.ProducerRef,
		"producer_name":     in.ProducerName,
		"product_ref":       in.ProductRef,
		"spec_name":         in.SpecName,
		"inspection_digest": in.InspectionDigest,
		"valid_until":       in.ValidUntil,
	}
	for k, v := range required {
		if strings.TrimSpace(v) == "" {
			return false, fmt.Errorf("%w: 缺少字段 %s", ErrInvalid, k)
		}
	}
	if _, perr := time.Parse(time.RFC3339, in.ValidUntil); perr != nil {
		return false, fmt.Errorf("%w: valid_until 必须是带偏移的 ISO 8601 时间", ErrInvalid)
	}
	if in.ContentDigest == "" {
		in.ContentDigest = digestAuthorization(in)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	switch err := tx.QueryRowContext(ctx,
		`SELECT content_digest FROM authorizations WHERE credential_id = ?`, in.CredentialID,
	).Scan(&existing); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case existing == in.ContentDigest:
		return false, nil // 同内容重复导入，幂等成功且不改动既有记录
	default:
		return false, fmt.Errorf("%w: 凭证 %s 已以不同内容收录，授权凭证不可修改", ErrConflict, in.CredentialID)
	}

	if _, err = tx.ExecContext(ctx, `
INSERT INTO authorizations (credential_id, producer_ref, producer_name, product_ref, spec_name,
                            batch_ref, inspection_digest, valid_until, issued_at, content_digest, received_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		in.CredentialID, in.ProducerRef, in.ProducerName, in.ProductRef, in.SpecName,
		nullIfBlank(in.BatchRef), in.InspectionDigest, in.ValidUntil, nullIfBlank(in.IssuedAt),
		in.ContentDigest, s.now().Format(time.RFC3339)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func digestAuthorization(in AuthorizationInput) string {
	canonical := struct {
		CredentialID     string `json:"credential_id"`
		ProducerRef      string `json:"producer_ref"`
		ProducerName     string `json:"producer_name"`
		ProductRef       string `json:"product_ref"`
		SpecName         string `json:"spec_name"`
		BatchRef         string `json:"batch_ref"`
		InspectionDigest string `json:"inspection_digest"`
		ValidUntil       string `json:"valid_until"`
		IssuedAt         string `json:"issued_at"`
	}{in.CredentialID, in.ProducerRef, in.ProducerName, in.ProductRef, in.SpecName,
		in.BatchRef, in.InspectionDigest, in.ValidUntil, in.IssuedAt}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 印制批次 → 箱件 → 交付
// ---------------------------------------------------------------------------

// RegisterPrintBatch 登记印制批次及该批预制的全部验证编号。
func (s *Store) RegisterPrintBatch(ctx context.Context, in PrintBatchInput) (*Result, error) {
	if blank(in.BatchID) || blank(in.PrinterRef) || blank(in.ProductRef) || blank(in.SpecName) {
		return nil, fmt.Errorf("%w: batch_id/printer_ref/product_ref/spec_name 均为必填", ErrInvalid)
	}
	codes, err := dedupeCodes(in.Codes)
	if err != nil {
		return nil, err
	}
	printedAt, err := s.parseTime(in.PrintedAt)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingProduct, existingSpec, existingPrinter, existingPrinterName sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT product_ref, spec_name, printer_ref, printer_name FROM print_batches WHERE batch_id = ?`,
		in.BatchID).
		Scan(&existingProduct, &existingSpec, &existingPrinter, &existingPrinterName)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `
INSERT INTO print_batches (batch_id, printer_ref, printer_name, product_ref, spec_name, printed_at, created_at)
VALUES (?,?,?,?,?,?,?)`,
			in.BatchID, in.PrinterRef, nullIfBlank(in.PrinterName), in.ProductRef, in.SpecName,
			printedAt, s.now().Format(time.RFC3339)); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if existingProduct.String != in.ProductRef || existingSpec.String != in.SpecName ||
			existingPrinter.String != in.PrinterRef {
			return nil, fmt.Errorf("%w: 印制批次 %s 已存在且内容不一致", ErrConflict, in.BatchID)
		}
	}

	affected, skipped, err := s.createLabels(ctx, tx, codes, labelSeed{
		printBatchID: in.BatchID,
		printerRef:   in.PrinterRef,
		printerName:  in.PrinterName,
		productRef:   in.ProductRef,
		specName:     in.SpecName,
		printedAt:    printedAt,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected, Skipped: skipped}, nil
}

type labelSeed struct {
	printBatchID            string
	printerRef, printerName string
	productRef, specName    string
	printedAt               string
}

func (s *Store) createLabels(ctx context.Context, tx *sql.Tx, codes []string, seed labelSeed) (affected, skipped []string, err error) {
	now := s.now().Format(time.RFC3339)
	for _, code := range codes {
		var status string
		switch scanErr := tx.QueryRowContext(ctx, `SELECT status FROM labels WHERE code = ?`, code).Scan(&status); {
		case errors.Is(scanErr, sql.ErrNoRows):
		case scanErr != nil:
			return nil, nil, scanErr
		default:
			// 重放：同批次内已存在的编号视为已处理；属于其他批次则构成编号冲突。
			var otherBatch string
			if e := tx.QueryRowContext(ctx, `SELECT print_batch_id FROM labels WHERE code = ?`, code).Scan(&otherBatch); e != nil {
				return nil, nil, e
			}
			if otherBatch != seed.printBatchID {
				return nil, nil, fmt.Errorf("%w: 编号 %s 已属于印制批次 %s，编号不得跨批复用", ErrConflict, code, otherBatch)
			}
			skipped = append(skipped, code)
			continue
		}
		if _, err = tx.ExecContext(ctx, `
INSERT INTO labels (code, print_batch_id, status, holder_type, holder_ref, holder_name,
                    product_ref, spec_name, created_at, updated_at)
VALUES (?,?, 'printed', 'printer', ?, ?, ?, ?, ?, ?)`,
			code, seed.printBatchID, seed.printerRef, nullIfBlank(seed.printerName),
			seed.productRef, seed.specName, now, now); err != nil {
			return nil, nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: "printed", actorType: "printer", actorRef: seed.printerRef,
			holderType: "printer", holderRef: seed.printerRef, holderName: seed.printerName,
			eventTime: seed.printedAt, payload: map[string]any{"print_batch_id": seed.printBatchID},
		}); err != nil {
			return nil, nil, err
		}
		affected = append(affected, code)
	}
	return affected, skipped, nil
}

// PackBox 将一批编号封入交付箱件。仅 printed（未装箱、未挂失）编号可封箱。
func (s *Store) PackBox(ctx context.Context, in BoxPackInput) (*Result, error) {
	if blank(in.BoxID) || blank(in.PrintBatchID) {
		return nil, fmt.Errorf("%w: box_id/print_batch_id 为必填", ErrInvalid)
	}
	codes, err := dedupeCodes(in.Codes)
	if err != nil {
		return nil, err
	}
	packedAt, err := s.parseTime(in.PackedAt)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var printerRef, printerName sql.NullString
	if err = s.requireBatch(ctx, tx, in.PrintBatchID, &printerRef, &printerName); err != nil {
		return nil, err
	}

	var existingCount int
	var existingBatch string
	switch err = tx.QueryRowContext(ctx, `SELECT print_batch_id, label_count FROM boxes WHERE box_id = ?`, in.BoxID).
		Scan(&existingBatch, &existingCount); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	case existingBatch != in.PrintBatchID || existingCount != len(codes):
		return nil, fmt.Errorf("%w: 箱件 %s 已存在且封装内容不一致", ErrConflict, in.BoxID)
	default:
		if sameSet, e := s.boxHasCodes(ctx, tx, in.BoxID, codes); e != nil {
			return nil, e
		} else if !sameSet {
			return nil, fmt.Errorf("%w: 箱件 %s 已存在且封装编号不一致", ErrConflict, in.BoxID)
		}
		return &Result{Skipped: codes}, tx.Commit() // 幂等重放
	}

	if _, err = tx.ExecContext(ctx, `
INSERT INTO boxes (box_id, print_batch_id, label_count, status, packed_at, created_at)
VALUES (?,?,?, 'packed', ?, ?)`,
		in.BoxID, in.PrintBatchID, len(codes), packedAt, s.now().Format(time.RFC3339)); err != nil {
		return nil, err
	}
	for _, code := range codes {
		l, e := s.loadLabel(ctx, tx, code)
		if e != nil {
			return nil, e
		}
		if l.printBatchID != in.PrintBatchID {
			return nil, fmt.Errorf("%w: 编号 %s 不属于印制批次 %s", ErrInvalid, code, in.PrintBatchID)
		}
		if l.status != "printed" {
			return nil, fmt.Errorf("%w: 编号 %s 当前状态 %s，不能封箱", ErrConflict, code, l.status)
		}
		if l.lostFlag {
			return nil, fmt.Errorf("%w: 编号 %s 已挂失，不能封箱", ErrConflict, code)
		}
		if _, err = tx.ExecContext(ctx,
			`UPDATE labels SET status='packed', box_id=?, updated_at=? WHERE code=?`,
			in.BoxID, s.now().Format(time.RFC3339), code); err != nil {
			return nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: "packed", actorType: "printer", actorRef: printerRef.String,
			holderType: "printer", holderRef: printerRef.String, holderName: printerName.String,
			eventTime: packedAt, payload: map[string]any{"box_id": in.BoxID},
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: codes}, nil
}

// DeliverBox 将箱件整箱交付给生产企业，箱内标签保管人转为该企业。
func (s *Store) DeliverBox(ctx context.Context, in BoxDeliverInput) (*Result, error) {
	if blank(in.BoxID) || blank(in.DeliveredToProducerRef) {
		return nil, fmt.Errorf("%w: box_id/delivered_to_producer_ref 为必填", ErrInvalid)
	}
	deliveredAt, err := s.parseTime(in.DeliveredAt)
	if err != nil {
		return nil, err
	}
	courierRef := in.CourierRef
	if blank(courierRef) {
		courierRef = in.DeliveredToProducerRef
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var batchID, status, alreadyTo sql.NullString
	if err = tx.QueryRowContext(ctx,
		`SELECT print_batch_id, status, delivered_to_producer_ref FROM boxes WHERE box_id = ?`, in.BoxID).
		Scan(&batchID, &status, &alreadyTo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 箱件 %s 不存在", ErrNotFound, in.BoxID)
		}
		return nil, err
	}
	if status.String == "delivered" {
		if alreadyTo.String != in.DeliveredToProducerRef {
			return nil, fmt.Errorf("%w: 箱件 %s 已交付给其他企业", ErrConflict, in.BoxID)
		}
		codes, e := s.codesInBox(ctx, tx, in.BoxID)
		if e != nil {
			return nil, e
		}
		return &Result{Skipped: codes}, tx.Commit()
	}

	if _, err = tx.ExecContext(ctx, `
UPDATE boxes SET status='delivered', delivered_to_producer_ref=?, delivered_to_producer_name=?, delivered_at=?
WHERE box_id=?`,
		in.DeliveredToProducerRef, nullIfBlank(in.DeliveredToProducerName), deliveredAt, in.BoxID); err != nil {
		return nil, err
	}
	codes, err := s.codesInBox(ctx, tx, in.BoxID)
	if err != nil {
		return nil, err
	}
	var affected, skipped []string
	for _, code := range codes {
		l, e := s.loadLabel(ctx, tx, code)
		if e != nil {
			return nil, e
		}
		if l.lostFlag {
			return nil, fmt.Errorf("%w: 编号 %s 处于挂失状态，须寻回或作废后再交付", ErrConflict, code)
		}
		if l.status == "delivered" && orBlank(l.holderRef) == in.DeliveredToProducerRef {
			skipped = append(skipped, code)
			continue
		}
		if l.status != "packed" {
			return nil, fmt.Errorf("%w: 编号 %s 当前状态 %s，不能交付", ErrConflict, code, l.status)
		}
		if _, err = tx.ExecContext(ctx, `
UPDATE labels SET status='delivered', holder_type='producer', holder_ref=?, holder_name=?, updated_at=?
WHERE code=?`,
			in.DeliveredToProducerRef, nullIfBlank(in.DeliveredToProducerName),
			s.now().Format(time.RFC3339), code); err != nil {
			return nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: "delivered", actorType: "courier", actorRef: courierRef,
			holderType: "producer", holderRef: in.DeliveredToProducerRef,
			holderName: in.DeliveredToProducerName, eventTime: deliveredAt,
			payload: map[string]any{"box_id": in.BoxID},
		}); err != nil {
			return nil, err
		}
		affected = append(affected, code)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected, Skipped: skipped}, nil
}

// ---------------------------------------------------------------------------
// 企业领用
// ---------------------------------------------------------------------------

// Issue 凭有效授权凭证领用标签。规格必须与标签印制规格一致，且授权在领用时点有效。
func (s *Store) Issue(ctx context.Context, in IssueInput) (*Result, error) {
	if blank(in.CredentialID) {
		return nil, fmt.Errorf("%w: credential_id 为必填", ErrInvalid)
	}
	codes, err := dedupeCodes(in.Codes)
	if err != nil {
		return nil, err
	}
	issuedAt, err := s.parseTime(in.IssuedAt)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	cred, err := s.loadCredential(ctx, tx, in.CredentialID)
	if err != nil {
		return nil, err
	}
	if !cred.validAt(s.now()) {
		return nil, fmt.Errorf("%w: 凭证 %s 已过有效期，剩余标签停止领用", ErrConflict, in.CredentialID)
	}

	var affected, skipped []string
	for _, code := range codes {
		l, e := s.loadLabel(ctx, tx, code)
		if e != nil {
			return nil, e
		}
		switch {
		case l.status == "issued" && l.credentialID.String == in.CredentialID:
			skipped = append(skipped, code) // 幂等重放
			continue
		case l.status != "delivered":
			return nil, fmt.Errorf("%w: 编号 %s 当前状态 %s，不能领用", ErrConflict, code, l.status)
		case l.lostFlag:
			return nil, fmt.Errorf("%w: 编号 %s 已挂失，不能领用", ErrConflict, code)
		case orBlank(l.holderType) != "producer" || orBlank(l.holderRef) != cred.producerRef:
			return nil, fmt.Errorf("%w: 编号 %s 不在企业 %s 保管之下", ErrConflict, code, cred.producerRef)
		case orBlank(l.productRef) != cred.productRef:
			return nil, fmt.Errorf("%w: 编号 %s 印制规格 %s 与凭证获准规格 %s 不符",
				ErrInvalid, code, orBlank(l.productRef), cred.productRef)
		}
		if _, err = tx.ExecContext(ctx, `
UPDATE labels SET status='issued', credential_id=?, producer_ref=?, producer_name=?,
                  product_ref=?, spec_name=?, updated_at=?
WHERE code=?`,
			cred.credentialID, cred.producerRef, cred.producerName, cred.productRef, cred.specName,
			s.now().Format(time.RFC3339), code); err != nil {
			return nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: "issued", actorType: "producer", actorRef: cred.producerRef,
			holderType: "producer", holderRef: cred.producerRef, holderName: cred.producerName,
			eventTime: issuedAt, payload: map[string]any{"credential_id": cred.credentialID},
		}); err != nil {
			return nil, err
		}
		affected = append(affected, code)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected, Skipped: skipped}, nil
}

// ---------------------------------------------------------------------------
// 离线设备贴附
// ---------------------------------------------------------------------------

// Apply 接收离线设备回传的贴附事件。整批事件以 (device_id, device_event_id) 去重：
// 同一事件重传返回首次结果；同键不同内容或把编号贴到另一个产品批次一律拒绝。
func (s *Store) Apply(ctx context.Context, in ApplyInput) (*Result, error) {
	if blank(in.DeviceID) || blank(in.DeviceEventID) {
		return nil, fmt.Errorf("%w: device_id/device_event_id 为必填，用于离线重传去重", ErrInvalid)
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", ErrInvalid)
	}
	eventTime, err := s.parseTime(in.EventTime)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, item := range in.Items {
		if blank(item.Code) || blank(item.ProductBatchRef) {
			return nil, fmt.Errorf("%w: 每条贴附记录必须包含 code 与 product_batch_ref", ErrInvalid)
		}
		if _, dup := seen[item.Code]; dup {
			return nil, fmt.Errorf("%w: 设备事件中编号 %s 重复", ErrInvalid, item.Code)
		}
		seen[item.Code] = struct{}{}
	}
	wantDigests := map[string]string{}
	for _, item := range in.Items {
		wantDigests[item.Code] = item.ProductBatchRef
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 首次见到该设备事件？若已收录，必须是同一事件的重传。
	rows, err := tx.QueryContext(ctx, `
SELECT code, product_batch_ref FROM label_events
WHERE device_id=? AND device_event_id=? ORDER BY id`, in.DeviceID, in.DeviceEventID)
	if err != nil {
		return nil, err
	}
	prior := map[string]string{}
	for rows.Next() {
		var c, b sql.NullString
		if err = rows.Scan(&c, &b); err != nil {
			rows.Close()
			return nil, err
		}
		prior[c.String] = b.String
	}
	rows.Close()
	if len(prior) > 0 {
		if len(prior) != len(wantDigests) {
			return nil, fmt.Errorf("%w: 设备事件 %s/%s 已以不同内容收录，禁止重复提交",
				ErrConflict, in.DeviceID, in.DeviceEventID)
		}
		for code, batch := range wantDigests {
			if prior[code] != batch {
				return nil, fmt.Errorf("%w: 编号 %s 已由设备事件贴附到批次 %s，不能改贴 %s",
					ErrConflict, code, prior[code], batch)
			}
		}
		replayed := make([]string, 0, len(prior))
		for code := range prior {
			replayed = append(replayed, code)
		}
		sort.Strings(replayed)
		return &Result{Skipped: replayed}, tx.Commit()
	}

	now := s.now().Format(time.RFC3339)
	var affected []string
	for _, item := range in.Items {
		l, e := s.loadLabel(ctx, tx, item.Code)
		if e != nil {
			return nil, e
		}
		switch {
		case l.status == "applied":
			// 设备事件键不同但编号已贴附：一个码不能贴到两个批次。
			return nil, fmt.Errorf("%w: 编号 %s 已贴附到批次 %s，不能重复贴附到 %s",
				ErrConflict, item.Code, l.productBatchRef.String, item.ProductBatchRef)
		case l.status == "void":
			return nil, fmt.Errorf("%w: 编号 %s 已作废，不能贴附", ErrConflict, item.Code)
		case l.status == "frozen":
			return nil, fmt.Errorf("%w: 编号 %s 已因授权到期冻结，剩余标签停止使用", ErrConflict, item.Code)
		case l.status != "issued":
			return nil, fmt.Errorf("%w: 编号 %s 当前状态 %s，不能贴附", ErrConflict, item.Code, l.status)
		case l.lostFlag:
			return nil, fmt.Errorf("%w: 编号 %s 已挂失，不能贴附", ErrConflict, item.Code)
		}
		active, e := s.scopeHasActiveAuthz(ctx, tx, l.producerRef.String, l.productRef.String)
		if e != nil {
			return nil, e
		}
		if !active {
			return nil, fmt.Errorf("%w: 编号 %s 对应授权已到期，未使用标签停止贴附", ErrConflict, item.Code)
		}
		if _, err = tx.ExecContext(ctx, `
UPDATE labels SET status='applied', product_batch_ref=?, applied_at=?, updated_at=?
WHERE code=?`,
			item.ProductBatchRef, eventTime, now, item.Code); err != nil {
			return nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: item.Code, eventType: "applied", actorType: "device", actorRef: in.DeviceID,
			holderType: "producer", holderRef: l.producerRef.String, holderName: l.producerName.String,
			deviceID: in.DeviceID, deviceEventID: in.DeviceEventID,
			productBatchRef: item.ProductBatchRef, eventTime: eventTime,
		}); err != nil {
			return nil, err
		}
		affected = append(affected, item.Code)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected}, nil
}

// ---------------------------------------------------------------------------
// 挂失 / 寻回 / 作废 / 到期冻结
// ---------------------------------------------------------------------------

// ReportLost 上报标签遗失。已贴附（售出）编号不允许挂失，以免把合规在售商品打成异常。
func (s *Store) ReportLost(ctx context.Context, in ReportInput) (*Result, error) {
	return s.reportFlag(ctx, in, true)
}

// ReportRecovered 解除挂失。冻结/作废等终态编号不可仅靠寻回复用。
func (s *Store) ReportRecovered(ctx context.Context, in ReportInput) (*Result, error) {
	return s.reportFlag(ctx, in, false)
}

func (s *Store) reportFlag(ctx context.Context, in ReportInput, lost bool) (*Result, error) {
	codes, err := dedupeCodes(in.Codes)
	if err != nil {
		return nil, err
	}
	actorType := in.ActorType
	if blank(actorType) {
		actorType = "producer"
	}
	if blank(in.ActorRef) {
		return nil, fmt.Errorf("%w: actor_ref 为必填", ErrInvalid)
	}
	eventAt, err := s.parseTime(in.EventTime)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var affected, skipped []string
	eventType := "lost"
	if !lost {
		eventType = "recovered"
	}
	for _, code := range codes {
		l, e := s.loadLabel(ctx, tx, code)
		if e != nil {
			return nil, e
		}
		if l.status == "void" || l.status == "frozen" {
			return nil, fmt.Errorf("%w: 编号 %s 已处于终态 %s", ErrConflict, code, l.status)
		}
		if lost && l.status == "applied" {
			return nil, fmt.Errorf("%w: 编号 %s 已贴附售出，不能挂失", ErrConflict, code)
		}
		if l.lostFlag == lost {
			skipped = append(skipped, code)
			continue
		}
		if lost {
			_, err = tx.ExecContext(ctx,
				`UPDATE labels SET lost_flag=1, lost_at=?, updated_at=? WHERE code=?`,
				eventAt, s.now().Format(time.RFC3339), code)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE labels SET lost_flag=0, lost_at=NULL, updated_at=? WHERE code=?`,
				s.now().Format(time.RFC3339), code)
		}
		if err != nil {
			return nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: eventType, actorType: actorType, actorRef: in.ActorRef,
			holderType: orBlank(l.holderType), holderRef: orBlank(l.holderRef), holderName: orBlank(l.holderName),
			eventTime: eventAt, payload: map[string]any{"note": in.Note, "actor_name": in.ActorName},
		}); err != nil {
			return nil, err
		}
		affected = append(affected, code)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected, Skipped: skipped}, nil
}

// Void 作废未使用标签（回收核销、损坏核销等）。已贴附售出编号不可作废，以保护既有销售。
func (s *Store) Void(ctx context.Context, in VoidInput) (*Result, error) {
	codes, err := dedupeCodes(in.Codes)
	if err != nil {
		return nil, err
	}
	reason := in.Reason
	if in.Recovered && blank(reason) {
		reason = "回收核销"
	}
	if blank(reason) {
		return nil, fmt.Errorf("%w: reason 为必填", ErrInvalid)
	}
	if blank(in.ActorRef) {
		in.ActorRef = "brand-admin"
	}
	eventAt, err := s.parseTime(in.EventTime)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var affected, skipped []string
	for _, code := range codes {
		l, e := s.loadLabel(ctx, tx, code)
		if e != nil {
			return nil, e
		}
		if l.status == "void" {
			skipped = append(skipped, code)
			continue
		}
		if l.status == "applied" {
			return nil, fmt.Errorf("%w: 编号 %s 已贴附售出，不能作废", ErrConflict, code)
		}
		if _, err = tx.ExecContext(ctx, `
UPDATE labels SET status='void', holder_type='system', holder_ref='brand-admin', holder_name='品牌管理方',
                  voided_at=?, void_reason=?, lost_flag=0, updated_at=?
WHERE code=?`,
			eventAt, reason, s.now().Format(time.RFC3339), code); err != nil {
			return nil, err
		}
		if err = s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: "voided", actorType: "brand-admin", actorRef: in.ActorRef,
			holderType: "system", holderRef: "brand-admin", holderName: "品牌管理方",
			eventTime: eventAt, payload: map[string]any{"reason": reason, "recovered": in.Recovered},
		}); err != nil {
			return nil, err
		}
		affected = append(affected, code)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected, Skipped: skipped}, nil
}

// FreezeExpired 冻结授权已到期范围（生产企业 + 获准规格）内尚未贴附的标签。
// 已贴附售出的编号不在其列——授权到期不溯及既往销售。
func (s *Store) FreezeExpired(ctx context.Context) (*Result, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	rows, err := tx.QueryContext(ctx,
		`SELECT producer_ref, product_ref FROM authorizations GROUP BY producer_ref, product_ref`)
	if err != nil {
		return nil, err
	}
	type scope struct{ producer, product string }
	var expired []scope
	for rows.Next() {
		var sc scope
		if err = rows.Scan(&sc.producer, &sc.product); err != nil {
			rows.Close()
			return nil, err
		}
		ok, e := s.scopeActiveInTx(ctx, tx, sc.producer, sc.product, now)
		if e != nil {
			rows.Close()
			return nil, e
		}
		if !ok {
			expired = append(expired, sc)
		}
	}
	rows.Close()

	at := now.Format(time.RFC3339)
	var affected []string
	freezeOne := func(code string) error {
		if _, err := tx.ExecContext(ctx, `
UPDATE labels SET status='frozen', frozen_at=?, holder_type='system', holder_ref='brand-admin',
                  holder_name='品牌管理方', updated_at=?
WHERE code=?`, at, s.now().Format(time.RFC3339), code); err != nil {
			return err
		}
		affected = append(affected, code)
		return s.appendEvent(ctx, tx, eventRow{
			code: code, eventType: "frozen", actorType: "system", actorRef: "scheduler",
			holderType: "system", holderRef: "brand-admin", holderName: "品牌管理方",
			eventTime: at, payload: map[string]any{"reason": "授权到期，剩余标签停止使用"},
		})
	}
	for _, sc := range expired {
		// 已领用未贴附：按企业 + 规格冻结。
		issued, err := s.codesByQuery(ctx, tx, `
SELECT code FROM labels
WHERE status='issued' AND producer_ref=? AND product_ref=? AND lost_flag=0`,
			sc.producer, sc.product)
		if err != nil {
			return nil, err
		}
		for _, code := range issued {
			if err = freezeOne(code); err != nil {
				return nil, err
			}
		}
		// 已交付未领用：标签尚未绑定凭证，按保管企业 + 箱内标签印制规格冻结。
		delivered, err := s.codesByQuery(ctx, tx, `
SELECT l.code FROM labels l
JOIN print_batches p ON p.batch_id = l.print_batch_id
WHERE l.status='delivered' AND l.holder_ref=? AND p.product_ref=? AND l.lost_flag=0`,
			sc.producer, sc.product)
		if err != nil {
			return nil, err
		}
		for _, code := range delivered {
			if err = freezeOne(code); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(affected)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Affected: affected}, nil
}

func (s *Store) codesByQuery(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var c string
		if err = rows.Scan(&c); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, rows.Err()
}

// ---------------------------------------------------------------------------
// 公众验真与品牌方溯源
// ---------------------------------------------------------------------------

// CredentialView 凭证视图（只读快照）。
type CredentialView struct {
	CredentialID       string `json:"credential_id"`
	ValidUntil         string `json:"valid_until"`
	AuthorizationState string `json:"authorization_state"` // active / expired
	InspectionDigest   string `json:"inspection_digest"`
}

// VerifyView 是采购商扫码所见。
type VerifyView struct {
	Code            string          `json:"code"`
	Valid           bool            `json:"valid"`
	Status          string          `json:"status"`
	AnomalyCode     string          `json:"anomaly_code,omitempty"`
	Anomaly         string          `json:"anomaly,omitempty"`
	ProducerName    string          `json:"producer_name,omitempty"`
	SpecName        string          `json:"spec_name,omitempty"`
	ProductRef      string          `json:"product_ref,omitempty"`
	ProductBatchRef string          `json:"product_batch_ref,omitempty"`
	AppliedAt       string          `json:"applied_at,omitempty"`
	PrintBatchID    string          `json:"print_batch_id"`
	Credential      *CredentialView `json:"credential,omitempty"`
	Note            string          `json:"note,omitempty"`
}

// Verify 返回编号当前对应的产品规格与异常标识。
func (s *Store) Verify(ctx context.Context, code string) (*VerifyView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	l, err := s.loadLabel(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	v := &VerifyView{
		Code:            code,
		Status:          l.status,
		ProducerName:    orBlank(l.producerName),
		SpecName:        orBlank(l.specName),
		ProductRef:      orBlank(l.productRef),
		ProductBatchRef: orBlank(l.productBatchRef),
		AppliedAt:       orBlank(l.appliedAt),
		PrintBatchID:    l.printBatchID,
	}
	if l.credentialID.Valid {
		cred, e := s.loadCredential(ctx, tx, l.credentialID.String)
		if e != nil {
			return nil, e
		}
		state := "active"
		// 贴附所用凭证本身可能到期；但若该企业+规格当前仍有有效凭证（已续期），整体授权视为有效。
		currentlyActive, e := s.scopeActiveInTx(ctx, tx, cred.producerRef, cred.productRef, s.now())
		if e != nil {
			return nil, e
		}
		if !currentlyActive {
			state = "expired"
		}
		v.Credential = &CredentialView{
			CredentialID: cred.credentialID, ValidUntil: cred.validUntil,
			AuthorizationState: state, InspectionDigest: cred.inspectionDigest,
		}
	}

	switch {
	case l.status == "void":
		v.AnomalyCode, v.Anomaly = "voided", "标签已作废，编号不得使用"
	case l.status == "frozen":
		v.AnomalyCode, v.Anomaly = "frozen", "标签因授权到期被冻结，未使用标签已停止使用"
	case l.lostFlag:
		v.AnomalyCode, v.Anomaly = "lost", "标签已被挂失，编号可能被挪用，请谨慎核验"
	case l.status == "applied":
		v.Valid = true
		if v.Credential != nil && v.Credential.AuthorizationState == "expired" &&
			appliedBeforeExpiry(orBlank(l.appliedAt), v.Credential.ValidUntil) {
			v.Note = "授权在验真时点已到期，但该标签贴附于到期前，不影响此件已售合规板材的验真结论"
		}
	case l.status == "issued" || l.status == "delivered" || l.status == "packed" || l.status == "printed":
		v.AnomalyCode, v.Anomaly = "not_applied", "标签尚未贴附到任何产品批次，疑似提前流出或挪用"
	}
	return v, nil
}

// EventView 是台账事件视图。
type EventView struct {
	EventType       string          `json:"event_type"`
	ActorType       string          `json:"actor_type"`
	ActorRef        string          `json:"actor_ref"`
	HolderType      string          `json:"holder_type,omitempty"`
	HolderRef       string          `json:"holder_ref,omitempty"`
	HolderName      string          `json:"holder_name,omitempty"`
	DeviceID        string          `json:"device_id,omitempty"`
	DeviceEventID   string          `json:"device_event_id,omitempty"`
	ProductBatchRef string          `json:"product_batch_ref,omitempty"`
	EventTime       string          `json:"event_time"`
	RecordedAt      string          `json:"recorded_at"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// LabelTrace 是品牌方对单个编号的全链路溯源。
type LabelTrace struct {
	Code          string      `json:"code"`
	Status        string      `json:"status"`
	Lost          bool        `json:"lost"`
	CurrentHolder Holder      `json:"current_holder"`
	LastCustodian Holder      `json:"last_custodian"`
	Label         LabelView   `json:"label"`
	Events        []EventView `json:"events"`
}

// Holder 保管人。
type Holder struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
	Name string `json:"name,omitempty"`
}

// LabelView 标签当前快照。
type LabelView struct {
	PrintBatchID    string `json:"print_batch_id"`
	BoxID           string `json:"box_id,omitempty"`
	ProducerRef     string `json:"producer_ref,omitempty"`
	ProducerName    string `json:"producer_name,omitempty"`
	ProductRef      string `json:"product_ref,omitempty"`
	SpecName        string `json:"spec_name,omitempty"`
	ProductBatchRef string `json:"product_batch_ref,omitempty"`
	CredentialID    string `json:"credential_id,omitempty"`
	AppliedAt       string `json:"applied_at,omitempty"`
	LostAt          string `json:"lost_at,omitempty"`
	FrozenAt        string `json:"frozen_at,omitempty"`
	VoidedAt        string `json:"voided_at,omitempty"`
	VoidReason      string `json:"void_reason,omitempty"`
}

// BatchTrace 是整批标签的去向汇总，支持品牌方追查整批遗失的最后保管人。
type BatchTrace struct {
	BatchID string          `json:"batch_id"`
	Counts  map[string]int  `json:"counts"`
	Total   int             `json:"total"`
	Lost    []LostLabelView `json:"lost_labels"`
}

// LostLabelView 描述挂失编号及其挂失时点的最后保管人。
type LostLabelView struct {
	Code          string `json:"code"`
	Status        string `json:"status"`
	BoxID         string `json:"box_id,omitempty"`
	LastCustodian Holder `json:"last_custodian"`
	LostAt        string `json:"lost_at"`
}

// TraceLabel 返回单个编号的完整台账。
func (s *Store) TraceLabel(ctx context.Context, code string) (*LabelTrace, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	l, err := s.loadLabel(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	events, err := s.eventsOf(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	cur := Holder{Type: orBlank(l.holderType), Ref: orBlank(l.holderRef), Name: orBlank(l.holderName)}
	last := cur
	// 挂失编号的"最后保管人"以挂失事件记录的保管人快照为准。
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].EventType == "lost" {
			last = Holder{
				Type: events[i].HolderType, Ref: events[i].HolderRef, Name: events[i].HolderName,
			}
			break
		}
	}
	return &LabelTrace{
		Code: code, Status: l.status, Lost: l.lostFlag,
		CurrentHolder: cur, LastCustodian: last,
		Label: LabelView{
			PrintBatchID: l.printBatchID, BoxID: orBlank(l.boxID),
			ProducerRef: orBlank(l.producerRef), ProducerName: orBlank(l.producerName),
			ProductRef: orBlank(l.productRef), SpecName: orBlank(l.specName),
			ProductBatchRef: orBlank(l.productBatchRef), CredentialID: orBlank(l.credentialID),
			AppliedAt: orBlank(l.appliedAt), LostAt: orBlank(l.lostAt),
			FrozenAt: orBlank(l.frozenAt), VoidedAt: orBlank(l.voidedAt), VoidReason: orBlank(l.voidReason),
		},
		Events: events,
	}, nil
}

// TraceBatch 汇总整批标签状态，并列明全部挂失编号及其最后保管人。
func (s *Store) TraceBatch(ctx context.Context, batchID string) (*BatchTrace, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM print_batches WHERE batch_id=?`, batchID).
		Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("%w: 印制批次 %s 不存在", ErrNotFound, batchID)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT code, status, lost_flag, COALESCE(box_id,''), COALESCE(lost_at,'')
FROM labels WHERE print_batch_id=? ORDER BY code`, batchID)
	if err != nil {
		return nil, err
	}
	type row struct {
		code, status, box, lostAt string
		lost                      bool
	}
	var rs []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.code, &r.status, &r.lost, &r.box, &r.lostAt); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()

	out := &BatchTrace{BatchID: batchID, Counts: map[string]int{}, Total: len(rs)}
	for _, r := range rs {
		out.Counts[r.status]++
		if !r.lost {
			continue
		}
		events, e := s.eventsOf(ctx, tx, r.code)
		if e != nil {
			return nil, e
		}
		custodian := Holder{}
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].EventType == "lost" {
				custodian = Holder{Type: events[i].HolderType, Ref: events[i].HolderRef, Name: events[i].HolderName}
				break
			}
		}
		out.Counts["lost_flagged"]++
		out.Lost = append(out.Lost, LostLabelView{
			Code: r.code, Status: r.status, BoxID: r.box,
			LastCustodian: custodian, LostAt: r.lostAt,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 内部辅助
// ---------------------------------------------------------------------------

type eventRow struct {
	code, eventType, actorType, actorRef string
	holderType, holderRef, holderName    string
	deviceID, deviceEventID              string
	productBatchRef                      string
	eventTime                            string
	payload                              map[string]any
}

func (s *Store) appendEvent(ctx context.Context, tx *sql.Tx, e eventRow) error {
	if e.eventTime == "" {
		e.eventTime = s.now().Format(time.RFC3339)
	}
	if len(e.payload) == 0 {
		e.payload = map[string]any{}
	}
	raw, err := json.Marshal(e.payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO label_events (code, event_type, actor_type, actor_ref, holder_type, holder_ref, holder_name,
                          device_id, device_event_id, product_batch_ref, event_time, recorded_at, payload)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.code, e.eventType, e.actorType, e.actorRef, nullIfBlank(e.holderType), nullIfBlank(e.holderRef),
		nullIfBlank(e.holderName), nullIfBlank(e.deviceID), nullIfBlank(e.deviceEventID),
		nullIfBlank(e.productBatchRef), e.eventTime, s.now().Format(time.RFC3339), string(raw))
	return err
}

type labelRow struct {
	code, status, printBatchID        string
	lostFlag                          bool
	boxID                             sql.NullString
	holderType, holderRef, holderName sql.NullString
	credentialID                      sql.NullString
	producerRef, producerName         sql.NullString
	productRef, specName              sql.NullString
	productBatchRef, appliedAt        sql.NullString
	lostAt, frozenAt                  sql.NullString
	voidedAt, voidReason              sql.NullString
}

func (s *Store) loadLabel(ctx context.Context, q querier, code string) (*labelRow, error) {
	if blank(code) {
		return nil, fmt.Errorf("%w: code 为空", ErrInvalid)
	}
	l := &labelRow{}
	err := q.QueryRowContext(ctx, `
SELECT code, print_batch_id, status, lost_flag, box_id, holder_type, holder_ref, holder_name,
       credential_id, producer_ref, producer_name, product_ref, spec_name,
       product_batch_ref, applied_at, lost_at, frozen_at, voided_at, void_reason
FROM labels WHERE code=?`, code).Scan(
		&l.code, &l.printBatchID, &l.status, &l.lostFlag, &l.boxID,
		&l.holderType, &l.holderRef, &l.holderName, &l.credentialID,
		&l.producerRef, &l.producerName, &l.productRef, &l.specName,
		&l.productBatchRef, &l.appliedAt, &l.lostAt, &l.frozenAt, &l.voidedAt, &l.voidReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 编号 %s 不存在", ErrNotFound, code)
	}
	return l, err
}

type credentialRow struct {
	credentialID, producerRef, producerName string
	productRef, specName                    string
	inspectionDigest, validUntil            string
}

func (s *Store) loadCredential(ctx context.Context, q querier, id string) (*credentialRow, error) {
	c := &credentialRow{}
	err := q.QueryRowContext(ctx, `
SELECT credential_id, producer_ref, producer_name, product_ref, spec_name, inspection_digest, valid_until
FROM authorizations WHERE credential_id=?`, id).Scan(
		&c.credentialID, &c.producerRef, &c.producerName, &c.productRef, &c.specName,
		&c.inspectionDigest, &c.validUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 凭证 %s 不存在", ErrNotFound, id)
	}
	return c, err
}

func (c *credentialRow) validAt(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, c.validUntil)
	if err != nil {
		return false
	}
	return !now.After(t)
}

func (s *Store) scopeHasActiveAuthz(ctx context.Context, tx *sql.Tx, producerRef, productRef string) (bool, error) {
	return s.scopeActiveInTx(ctx, tx, producerRef, productRef, s.now())
}

func (s *Store) scopeActiveInTx(ctx context.Context, q querier, producerRef, productRef string, now time.Time) (bool, error) {
	rows, err := q.QueryContext(ctx, `
SELECT valid_until FROM authorizations WHERE producer_ref=? AND product_ref=?`,
		producerRef, productRef)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	active := false
	for rows.Next() {
		var validUntil string
		if err = rows.Scan(&validUntil); err != nil {
			return false, err
		}
		t, perr := time.Parse(time.RFC3339, validUntil)
		if perr != nil {
			continue
		}
		if !now.After(t) {
			active = true
		}
	}
	return active, rows.Err()
}

func (s *Store) requireBatch(ctx context.Context, tx *sql.Tx, batchID string, printerRef, printerName *sql.NullString) error {
	err := tx.QueryRowContext(ctx, `SELECT printer_ref, printer_name FROM print_batches WHERE batch_id=?`, batchID).
		Scan(printerRef, printerName)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 印制批次 %s 不存在", ErrNotFound, batchID)
	}
	return err
}

func (s *Store) codesInBox(ctx context.Context, tx *sql.Tx, boxID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT code FROM labels WHERE box_id=? ORDER BY code`, boxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var c string
		if err = rows.Scan(&c); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, rows.Err()
}

func (s *Store) boxHasCodes(ctx context.Context, tx *sql.Tx, boxID string, codes []string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM labels WHERE box_id=?`, boxID).Scan(&n); err != nil {
		return false, err
	}
	if n != len(codes) {
		return false, nil
	}
	for _, c := range codes {
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT box_id FROM labels WHERE code=?`, c).Scan(&owner); err != nil {
			return false, err
		}
		if owner != boxID {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) eventsOf(ctx context.Context, tx *sql.Tx, code string) ([]EventView, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT event_type, actor_type, actor_ref, COALESCE(holder_type,''), COALESCE(holder_ref,''),
       COALESCE(holder_name,''), COALESCE(device_id,''), COALESCE(device_event_id,''),
       COALESCE(product_batch_ref,''), event_time, recorded_at, payload
FROM label_events WHERE code=? ORDER BY id`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventView
	for rows.Next() {
		var e EventView
		var payload string
		if err = rows.Scan(&e.EventType, &e.ActorType, &e.ActorRef, &e.HolderType, &e.HolderRef,
			&e.HolderName, &e.DeviceID, &e.DeviceEventID, &e.ProductBatchRef,
			&e.EventTime, &e.RecordedAt, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) parseTime(v string) (string, error) {
	if blank(v) {
		return s.now().Format(time.RFC3339), nil
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return "", fmt.Errorf("%w: 时间字段必须是带偏移的 ISO 8601 字符串: %v", ErrInvalid, err)
	}
	return v, nil
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func dedupeCodes(codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, fmt.Errorf("%w: codes 不能为空", ErrInvalid)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("%w: 编号不能为空", ErrInvalid)
		}
		if _, ok := seen[c]; ok {
			return nil, fmt.Errorf("%w: 编号 %s 在请求中重复", ErrInvalid, c)
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

func blank(v string) bool { return strings.TrimSpace(v) == "" }

// appliedBeforeExpiry 判断贴附是否发生在所用凭证到期之前（两者都是带偏移 ISO 8601）。
func appliedBeforeExpiry(appliedAt, validUntil string) bool {
	a, err1 := time.Parse(time.RFC3339, appliedAt)
	u, err2 := time.Parse(time.RFC3339, validUntil)
	return err1 == nil && err2 == nil && !a.After(u)
}

func nullIfBlank(v string) any {
	if blank(v) {
		return nil
	}
	return v
}

func orBlank(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

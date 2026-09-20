package domain

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

// harness 封装测试用数据库与服务。
type harness struct {
	t   *testing.T
	st  *store.Store
	svc *Service
	ctx context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite3")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixed := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return fixed })
	return &harness{t: t, st: st, svc: New(st), ctx: context.Background()}
}

const (
	validFrom  = "2026-01-01T00:00:00+08:00"
	validUntil = "2026-12-31T23:59:59+08:00"
)

func mustAttestation(t *testing.T, h *harness, producer, spec, until string) *Attestation {
	t.Helper()
	a, err := h.svc.ImportAttestation(h.ctx, ImportAttestationInput{
		SourceRef:        "AUTH-" + producer + "-" + spec,
		ProducerRef:      producer,
		ProductRef:       "PROD-" + spec,
		BatchRef:         "BATCH-REF-1",
		Spec:             spec,
		InspectionDigest: "sha256:" + spec,
		ValidFrom:        validFrom,
		ValidUntil:       until,
	}, "brand-admin")
	if err != nil {
		t.Fatalf("导入授权凭证: %v", err)
	}
	return a
}

// seedHappyPath 建立 凭证→印制→装箱→交付→领用→产品批次 的完整前置，返回可用编号与产品批次。
func seedHappyPath(t *testing.T, h *harness, codes []string) (producer string, attID string, productBatchID string) {
	t.Helper()
	producer = "MAKER-001"
	att := mustAttestation(t, h, producer, "1220x2440x18-E0", validUntil)

	pb, err := h.svc.RegisterPrintBatch(h.ctx, RegisterPrintBatchInput{
		PrinterRef: "PRINTER-A",
		PrintedAt:  "2026-08-01T09:00:00+08:00",
		Codes:      codes,
	}, "printer-a")
	if err != nil {
		t.Fatalf("登记印制批次: %v", err)
	}

	carton, err := h.svc.PackCarton(h.ctx, PackCartonInput{
		PrintBatchID: pb.ID,
		Codes:        codes,
	}, "printer-a")
	if err != nil {
		t.Fatalf("装箱: %v", err)
	}

	if _, err := h.svc.CreateDelivery(h.ctx, CreateDeliveryInput{
		ProducerRef: producer,
		CartonIDs:   []string{carton.ID},
		DeliveredAt: "2026-08-10T10:00:00+08:00",
	}, "logistics"); err != nil {
		t.Fatalf("交付: %v", err)
	}

	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef:   producer,
		AttestationID: att.ID,
		Codes:         codes,
		IssuedAt:      "2026-08-15T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("领用: %v", err)
	}

	pbatch, err := h.svc.RegisterProductBatch(h.ctx, RegisterProductBatchInput{
		ProducerRef:   producer,
		AttestationID: att.ID,
		ProductRef:    "PROD-1220x2440x18-E0",
		ProducedAt:    "2026-09-01T10:00:00+08:00",
	}, producer)
	if err != nil {
		t.Fatalf("登记产品批次: %v", err)
	}
	return producer, att.ID, pbatch.ID
}

func TestHappyPathLifecycleAndVerify(t *testing.T) {
	h := newHarness(t)
	codes := []string{"CXLP-0001", "CXLP-0002"}
	producer, _, pbatch := seedHappyPath(t, h, codes)

	res, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code:           codes[0],
		ProductBatchID: pbatch,
		DeviceRef:      "GUN-7",
		ClientEventID:  "evt-1",
		OccurredAt:     "2026-09-02T09:30:00+08:00",
	}, producer)
	if err != nil {
		t.Fatalf("贴附: %v", err)
	}
	if res.Replayed {
		t.Fatal("首次贴附不应是重放")
	}

	v, err := h.svc.Verify(h.ctx, codes[0])
	if err != nil {
		t.Fatalf("验证: %v", err)
	}
	if !v.Valid || v.Abnormal {
		t.Fatalf("已贴附合规产品应验证通过，得到 %+v", v)
	}
	if v.Spec != "1220x2440x18-E0" || v.ProducerRef != producer {
		t.Fatalf("验证应展示规格与企业，得到 %+v", v)
	}
}

func TestOfflineRetransmitIsIdempotent(t *testing.T) {
	h := newHarness(t)
	codes := []string{"CXLP-1001"}
	producer, _, pbatch := seedHappyPath(t, h, codes)

	in := AttachInput{Code: codes[0], ProductBatchID: pbatch, DeviceRef: "GUN-7",
		ClientEventID: "evt-offline-1", OccurredAt: "2026-09-02T09:30:00+08:00"}
	if _, err := h.svc.AttachCode(h.ctx, in, producer); err != nil {
		t.Fatalf("首次贴附: %v", err)
	}
	// 离线设备恢复网络后原样重传。
	replay, err := h.svc.AttachCode(h.ctx, in, producer)
	if err != nil {
		t.Fatalf("重传应幂等成功: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("重传应标记 replayed=true")
	}
	var n int
	if err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM attachments WHERE code=?`, codes[0]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("重传不得产生第二条贴附记录，实际 %d 条", n)
	}

	// 同一幂等键挪用到别的编号/批次必须拒绝。
	bad := in
	bad.Code = "CXLP-1002"
	if _, err := h.svc.AttachCode(h.ctx, bad, producer); !errors.Is(err, ErrConflict) {
		t.Fatalf("幂等键挪用应冲突，得到 %v", err)
	}
}

func TestOneCodeCannotAttachTwoBatches(t *testing.T) {
	h := newHarness(t)
	producer := "MAKER-002"
	att := mustAttestation(t, h, producer, "1220x2440x18-E0", validUntil)

	pb1, err := h.svc.RegisterProductBatch(h.ctx, RegisterProductBatchInput{
		ProducerRef: producer, AttestationID: att.ID,
		ProductRef: "PROD-1220x2440x18-E0", ProducedAt: "2026-09-01T10:00:00+08:00",
	}, producer)
	if err != nil {
		t.Fatal(err)
	}
	pb2, err := h.svc.RegisterProductBatch(h.ctx, RegisterProductBatchInput{
		ProducerRef: producer, AttestationID: att.ID,
		ProductRef: "PROD-1220x2440x18-E0", ProducedAt: "2026-09-03T10:00:00+08:00",
	}, producer)
	if err != nil {
		t.Fatal(err)
	}

	codes := []string{"CXLP-2001", "CXLP-2002"}
	printed, err := h.svc.RegisterPrintBatch(h.ctx, RegisterPrintBatchInput{
		PrinterRef: "PRINTER-A", PrintedAt: "2026-08-01T09:00:00+08:00", Codes: codes,
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	carton, err := h.svc.PackCarton(h.ctx, PackCartonInput{PrintBatchID: printed.ID, Codes: codes}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateDelivery(h.ctx, CreateDeliveryInput{
		ProducerRef: producer, CartonIDs: []string{carton.ID}, DeliveredAt: "2026-08-10T10:00:00+08:00",
	}, "l"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: producer, AttestationID: att.ID, Codes: codes, IssuedAt: "2026-08-15T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code: codes[0], ProductBatchID: pb1.ID, ClientEventID: "e-a", OccurredAt: "2026-09-02T08:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("贴附第一批: %v", err)
	}
	// 同一编号试图贴到第二个不同批次——即使设备换了事件键也必须失败。
	if _, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code: codes[0], ProductBatchID: pb2.ID, ClientEventID: "e-b", OccurredAt: "2026-09-04T08:00:00+08:00",
	}, producer); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "两个不同批次") {
		t.Fatalf("一码两批次应明确冲突，得到 %v", err)
	}
}

func TestExpiredAttestationDoesNotRetroactivelyFakify(t *testing.T) {
	h := newHarness(t)
	producer := "MAKER-003"
	// 授权 8 月底到期。
	att := mustAttestation(t, h, producer, "1220x2440x15-E1", "2026-08-31T23:59:59+08:00")

	codes := []string{"CXLP-3001", "CXLP-3002", "CXLP-3003"}
	printed, err := h.svc.RegisterPrintBatch(h.ctx, RegisterPrintBatchInput{
		PrinterRef: "PRINTER-A", PrintedAt: "2026-07-01T09:00:00+08:00", Codes: codes,
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	carton, err := h.svc.PackCarton(h.ctx, PackCartonInput{PrintBatchID: printed.ID, Codes: codes}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateDelivery(h.ctx, CreateDeliveryInput{
		ProducerRef: producer, CartonIDs: []string{carton.ID}, DeliveredAt: "2026-07-10T10:00:00+08:00",
	}, "l"); err != nil {
		t.Fatal(err)
	}

	// 有效期内领用 3001。
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: producer, AttestationID: att.ID, Codes: []string{codes[0]}, IssuedAt: "2026-07-15T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("有效期内领用: %v", err)
	}
	// 到期后领用剩余标签 → 拒绝。
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: producer, AttestationID: att.ID, Codes: []string{codes[1]}, IssuedAt: "2026-09-05T10:00:00+08:00",
	}, producer); !errors.Is(err, ErrAttestExpired) {
		t.Fatalf("过期授权领用应拒绝，得到 %v", err)
	}

	pbatch, err := h.svc.RegisterProductBatch(h.ctx, RegisterProductBatchInput{
		ProducerRef: producer, AttestationID: att.ID, ProductRef: "PROD-1220x2440x15-E1",
		ProducedAt: "2026-08-20T10:00:00+08:00",
	}, producer)
	if err != nil {
		t.Fatal(err)
	}
	// 到期前（8 月）已贴附、已售出。
	if _, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code: codes[0], ProductBatchID: pbatch.ID, ClientEventID: "e-sold", OccurredAt: "2026-08-25T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("有效期内贴附: %v", err)
	}

	// 到期后停用未领用的剩余标签（3002、3003 仍为 delivered）。
	n, err := h.svc.BlockExpiredUnused(h.ctx, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC), "brand-admin")
	if err != nil {
		t.Fatalf("停用: %v", err)
	}
	if n != 2 {
		t.Fatalf("应停用 2 个未领用标签，实际 %d", n)
	}

	// 已售出板材仍然验真通过，不被追溯判定为假货。
	v, err := h.svc.Verify(h.ctx, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid {
		t.Fatalf("授权到期前售出的合规板材不应变成假货: %+v", v)
	}
	found := false
	for _, r := range v.Reasons {
		if r == "attestation_expired_after_attach" {
			found = true
		}
	}
	if !found {
		t.Fatalf("可提示授权在贴附后到期，但不影响验真，得到 %+v", v)
	}

	// 未领用标签验证为异常停用。
	v2, err := h.svc.Verify(h.ctx, codes[1])
	if err != nil {
		t.Fatal(err)
	}
	if v2.Valid || !v2.Abnormal || v2.Status != "blocked" {
		t.Fatalf("未领用剩余标签应为 blocked 异常，得到 %+v", v2)
	}

	// 停用码禁止领用/贴附。
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: producer, AttestationID: att.ID, Codes: []string{codes[1]}, IssuedAt: "2026-09-06T10:00:00+08:00",
	}, producer); !errors.Is(err, ErrConflict) {
		t.Fatalf("停用码领用应冲突，得到 %v", err)
	}
}

func TestAttachAfterExpiryRejected(t *testing.T) {
	h := newHarness(t)
	producer := "MAKER-004"
	att := mustAttestation(t, h, producer, "1220x2440x18-E0", "2026-08-31T23:59:59+08:00")
	codes := []string{"CXLP-4001"}
	printed, err := h.svc.RegisterPrintBatch(h.ctx, RegisterPrintBatchInput{
		PrinterRef: "PRINTER-A", PrintedAt: "2026-07-01T09:00:00+08:00", Codes: codes,
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	carton, err := h.svc.PackCarton(h.ctx, PackCartonInput{PrintBatchID: printed.ID, Codes: codes}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateDelivery(h.ctx, CreateDeliveryInput{
		ProducerRef: producer, CartonIDs: []string{carton.ID}, DeliveredAt: "2026-07-10T10:00:00+08:00",
	}, "l"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: producer, AttestationID: att.ID, Codes: codes, IssuedAt: "2026-07-15T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatal(err)
	}
	pbatch, err := h.svc.RegisterProductBatch(h.ctx, RegisterProductBatchInput{
		ProducerRef: producer, AttestationID: att.ID, ProductRef: "PROD-1220x2440x18-E0",
		ProducedAt: "2026-08-20T10:00:00+08:00",
	}, producer)
	if err != nil {
		t.Fatal(err)
	}
	// 设备离线到 9 月才上报一笔实际发生在 9 月（授权到期后）的贴附 → 拒绝。
	if _, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code: codes[0], ProductBatchID: pbatch.ID, ClientEventID: "late", OccurredAt: "2026-09-02T10:00:00+08:00",
	}, producer); !errors.Is(err, ErrAttestExpired) {
		t.Fatalf("到期后发生的贴附应拒绝，得到 %v", err)
	}
}

func TestLostLabelTraceAndCustodianSummary(t *testing.T) {
	h := newHarness(t)
	codes := []string{"CXLP-5001", "CXLP-5002"}
	producer, _, pbatch := seedHappyPath(t, h, codes)

	// 5001 已贴附后遗失；5002 仍在企业领用保管期间遗失。
	if _, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code: codes[0], ProductBatchID: pbatch, ClientEventID: "e5001", OccurredAt: "2026-09-02T09:00:00+08:00",
	}, producer); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.ReportLost(h.ctx, codes[0], LostInput{
		Note: "成品仓失窃", ReportedAt: "2026-09-03T09:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("挂失 5001: %v", err)
	}
	if err := h.svc.ReportLost(h.ctx, codes[1], LostInput{
		ReportedAt: "2026-09-03T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("挂失 5002: %v", err)
	}

	// 公开验证：遗失码异常，且不展示企业名称。
	v, err := h.svc.Verify(h.ctx, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !v.Abnormal || v.Status != "lost" || v.ProducerRef != "" {
		t.Fatalf("遗失码应异常且不暴露企业名，得到 %+v", v)
	}

	// 保管链：5001 挂失时最后保管方为产品批次（on_product），5002 为生产企业。
	tr, err := h.svc.Trace(h.ctx, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	var lostEvt *CustodyEvent
	for i := range tr.Events {
		if tr.Events[i].EventType == "reported_lost" {
			lostEvt = &tr.Events[i]
		}
	}
	if lostEvt == nil {
		t.Fatal("保管链缺少挂失事件")
	}

	var printBatchID string
	if err := h.st.DB.QueryRow(`SELECT print_batch_id FROM codes WHERE code=?`, codes[0]).Scan(&printBatchID); err != nil {
		t.Fatal(err)
	}
	summary, err := h.svc.LostCustodians(h.ctx, printBatchID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.LostTotal != 2 || summary.OpenLostTotal != 2 {
		t.Fatalf("整批遗失应为 2/2，得到 %+v", summary)
	}
	got := map[string]int{}
	for _, c := range summary.ByLastCustodian {
		got[c.CustodianType+":"+c.CustodianRef] += c.Count
	}
	if got["on_product:"+pbatch] != 1 {
		t.Fatalf("应有 1 个遗失码最后在产品批次上，得到 %+v", got)
	}
	if got["producer:"+producer] != 1 {
		t.Fatalf("应有 1 个遗失码最后由企业保管，得到 %+v", got)
	}

	// 找回后恢复到挂失前状态（5002 → issued）。
	if err := h.svc.RecoverLost(h.ctx, codes[1], "2026-09-04T09:00:00+08:00", producer); err != nil {
		t.Fatalf("找回: %v", err)
	}
	var status string
	if err := h.st.DB.QueryRow(`SELECT status FROM codes WHERE code=?`, codes[1]).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "issued" {
		t.Fatalf("找回后应恢复 issued，得到 %s", status)
	}
}

func TestVerifyUnknownAndUnattachedHidesProducer(t *testing.T) {
	h := newHarness(t)

	v, err := h.svc.Verify(h.ctx, "FAKE-9999")
	if err != nil {
		t.Fatal(err)
	}
	if v.Valid || !v.Abnormal || v.Status != "unknown" {
		t.Fatalf("不存在的编号应判异常，得到 %+v", v)
	}

	codes := []string{"CXLP-6001"}
	producer, _, _ := seedHappyPath(t, h, codes)
	// 已领用但未贴附：不显示企业名，提示可能是挪用/遗失标签。
	v2, err := h.svc.Verify(h.ctx, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !v2.Abnormal || v2.ProducerRef != "" || v2.ProducerRef == producer {
		t.Fatalf("未贴附标签不得显示企业名称，得到 %+v", v2)
	}
}

func TestSpecMismatchRejected(t *testing.T) {
	h := newHarness(t)
	producer := "MAKER-007"
	att18 := mustAttestation(t, h, producer, "SPEC-18MM", validUntil)
	att15 := mustAttestation(t, h, producer, "SPEC-15MM", validUntil)

	codes := []string{"CXLP-7001"}
	printed, err := h.svc.RegisterPrintBatch(h.ctx, RegisterPrintBatchInput{
		PrinterRef: "PRINTER-A", PrintedAt: "2026-08-01T09:00:00+08:00", Codes: codes,
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	carton, err := h.svc.PackCarton(h.ctx, PackCartonInput{PrintBatchID: printed.ID, Codes: codes}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateDelivery(h.ctx, CreateDeliveryInput{
		ProducerRef: producer, CartonIDs: []string{carton.ID}, DeliveredAt: "2026-08-10T10:00:00+08:00",
	}, "l"); err != nil {
		t.Fatal(err)
	}
	// 按 18mm 授权领用，规格被锁定。
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: producer, AttestationID: att18.ID, Codes: codes, IssuedAt: "2026-08-15T10:00:00+08:00",
	}, producer); err != nil {
		t.Fatal(err)
	}
	// 试图贴到 15mm 规格的产品批次 → 拒绝。
	pbatch15, err := h.svc.RegisterProductBatch(h.ctx, RegisterProductBatchInput{
		ProducerRef: producer, AttestationID: att15.ID, ProductRef: "PROD-SPEC-15MM",
		ProducedAt: "2026-09-01T10:00:00+08:00",
	}, producer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AttachCode(h.ctx, AttachInput{
		Code: codes[0], ProductBatchID: pbatch15.ID, ClientEventID: "e-spec", OccurredAt: "2026-09-02T10:00:00+08:00",
	}, producer); !errors.Is(err, ErrAttestMismatch) {
		t.Fatalf("规格不符应拒绝，得到 %v", err)
	}
}

func TestAttestationsAreImmutable(t *testing.T) {
	h := newHarness(t)
	att := mustAttestation(t, h, "MAKER-008", "SPEC-X", validUntil)

	if _, err := h.st.DB.Exec(`UPDATE attestations SET valid_until=? WHERE id=?`,
		"2030-01-01T00:00:00Z", att.ID); err == nil {
		t.Fatal("授权凭证必须禁止 UPDATE")
	}
	if _, err := h.st.DB.Exec(`DELETE FROM attestations WHERE id=?`, att.ID); err == nil {
		t.Fatal("授权凭证必须禁止 DELETE")
	}
	// 重复导入同一外部记录号 → 冲突而非覆盖。
	if _, err := h.svc.ImportAttestation(h.ctx, ImportAttestationInput{
		SourceRef: "AUTH-MAKER-008-SPEC-X", ProducerRef: "MAKER-008", ProductRef: "PROD-SPEC-X",
		BatchRef: "b", Spec: "SPEC-X", InspectionDigest: "d", ValidFrom: validFrom, ValidUntil: validUntil,
	}, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复外部凭证应冲突，得到 %v", err)
	}
}

func TestVoidCode(t *testing.T) {
	h := newHarness(t)
	codes := []string{"CXLP-8001"}
	producer, _, _ := seedHappyPath(t, h, codes)
	if err := h.svc.VoidCode(h.ctx, codes[0], VoidInput{
		Reason: "damaged", Note: "印刷污损", VoidedAt: "2026-09-02T18:00:00+08:00",
	}, producer); err != nil {
		t.Fatalf("作废: %v", err)
	}
	v, err := h.svc.Verify(h.ctx, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	if v.Valid || !v.Abnormal || v.Status != "void" {
		t.Fatalf("作废码应异常，得到 %+v", v)
	}
	if err := h.svc.VoidCode(h.ctx, codes[0], VoidInput{
		Reason: "admin", VoidedAt: "2026-09-03T18:00:00+08:00",
	}, producer); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复作废应冲突，得到 %v", err)
	}
}

func TestIssueRejectsForeignDelivery(t *testing.T) {
	h := newHarness(t)
	// A 企业交付的码，B 企业不能凭自己的授权领用。
	codes := []string{"CXLP-9001"}
	printed, err := h.svc.RegisterPrintBatch(h.ctx, RegisterPrintBatchInput{
		PrinterRef: "PRINTER-A", PrintedAt: "2026-08-01T09:00:00+08:00", Codes: codes,
	}, "p")
	if err != nil {
		t.Fatal(err)
	}
	carton, err := h.svc.PackCarton(h.ctx, PackCartonInput{PrintBatchID: printed.ID, Codes: codes}, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateDelivery(h.ctx, CreateDeliveryInput{
		ProducerRef: "MAKER-A", CartonIDs: []string{carton.ID}, DeliveredAt: "2026-08-10T10:00:00+08:00",
	}, "l"); err != nil {
		t.Fatal(err)
	}
	attB := mustAttestation(t, h, "MAKER-B", "SPEC-B", validUntil)
	if _, err := h.svc.IssueCodes(h.ctx, IssueInput{
		ProducerRef: "MAKER-B", AttestationID: attB.ID, Codes: codes, IssuedAt: "2026-08-15T10:00:00+08:00",
	}, "MAKER-B"); !errors.Is(err, ErrAttestMismatch) {
		t.Fatalf("非持有企业领用应拒绝，得到 %v", err)
	}
}

var _ = sql.ErrNoRows

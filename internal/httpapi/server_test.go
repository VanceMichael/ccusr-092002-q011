package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vancemichael/092002-forest-brand-attest/internal/domain"
	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

func newRouter(t *testing.T) (http.Handler, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "http.sqlite3"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	svc := domain.New(st)
	return Router(st, svc), func() { _ = st.Close() }
}

func doJSON(t *testing.T, h http.Handler, method, target, idemKey, actor string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if actor != "" {
		req.Header.Set("X-Actor", actor)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestHealth(t *testing.T) {
	h, cleanup := newRouter(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("健康接口状态码为 %d", rec.Code)
	}
}

// TestHTTPEndToEnd 覆盖 凭证→印制→装箱→交付→领用→产品批次→贴附→验证→挂失→追溯 的 HTTP 全链路。
func TestHTTPEndToEnd(t *testing.T) {
	h, cleanup := newRouter(t)
	defer cleanup()

	attestationBody := map[string]any{
		"source_ref":        "AUTH-MAKER-001",
		"producer_ref":      "MAKER-001",
		"product_ref":       "PROD-E0",
		"batch_ref":         "WB-1",
		"spec":              "1220x2440x18-E0",
		"inspection_digest": "sha256:abc",
		"valid_from":        "2026-01-01T00:00:00+08:00",
		"valid_until":       "2026-12-31T23:59:59+08:00",
	}
	code, body := doJSON(t, h, "POST", "/v1/admin/attestations", "att-1", "brand", attestationBody)
	if code != http.StatusCreated {
		t.Fatalf("导入凭证 %d: %v", code, body)
	}
	attID, _ := body["id"].(string)

	// 同键同负载重放 → 返回首次的 201。
	if code, body := doJSON(t, h, "POST", "/v1/admin/attestations", "att-1", "brand", attestationBody); code != http.StatusCreated {
		t.Fatalf("同键同负载重放应返回原始 201，得到 %d: %v", code, body)
	}

	code, body = doJSON(t, h, "POST", "/v1/admin/print-batches", "pb-1", "printer-a", map[string]any{
		"printer_ref": "PRINTER-A",
		"printed_at":  "2026-08-01T09:00:00+08:00",
		"codes":       []string{"CXLP-H-001", "CXLP-H-002"},
	})
	if code != http.StatusCreated {
		t.Fatalf("印制 %d: %v", code, body)
	}
	printBatchID, _ := body["id"].(string)

	code, body = doJSON(t, h, "POST", "/v1/admin/cartons", "ct-1", "printer-a", map[string]any{
		"print_batch_id": printBatchID,
		"codes":          []string{"CXLP-H-001", "CXLP-H-002"},
	})
	if code != http.StatusCreated {
		t.Fatalf("装箱 %d: %v", code, body)
	}
	cartonID, _ := body["id"].(string)

	code, body = doJSON(t, h, "POST", "/v1/admin/deliveries", "dl-1", "logistics", map[string]any{
		"producer_ref": "MAKER-001",
		"carton_ids":   []string{cartonID},
		"delivered_at": "2026-08-10T10:00:00+08:00",
	})
	if code != http.StatusCreated {
		t.Fatalf("交付 %d: %v", code, body)
	}

	code, body = doJSON(t, h, "POST", "/v1/admin/issues", "is-1", "MAKER-001", map[string]any{
		"producer_ref":   "MAKER-001",
		"attestation_id": attID,
		"codes":          []string{"CXLP-H-001", "CXLP-H-002"},
		"issued_at":      "2026-08-15T10:00:00+08:00",
	})
	if code != http.StatusCreated {
		t.Fatalf("领用 %d: %v", code, body)
	}

	code, body = doJSON(t, h, "POST", "/v1/admin/product-batches", "pbat-1", "MAKER-001", map[string]any{
		"producer_ref":   "MAKER-001",
		"attestation_id": attID,
		"product_ref":    "PROD-E0",
		"produced_at":    "2026-09-01T10:00:00+08:00",
	})
	if code != http.StatusCreated {
		t.Fatalf("产品批次 %d: %v", code, body)
	}
	pbatchID, _ := body["id"].(string)

	attachBody := map[string]any{
		"code":             "CXLP-H-001",
		"product_batch_id": pbatchID,
		"device_ref":       "GUN-7",
		"client_event_id":  "evt-http-1",
		"occurred_at":      "2026-09-02T09:30:00+08:00",
	}
	if code, body = doJSON(t, h, "POST", "/v1/attachments", "attach-1", "GUN-7", attachBody); code != http.StatusCreated {
		t.Fatalf("首次贴附 %d: %v", code, body)
	}
	// 离线重传：相同 Idempotency-Key 与负载 → 200 且幂等重放标记。
	req := httptest.NewRequest(http.MethodPost, "/v1/attachments",
		bytes.NewReader(mustJSON(attachBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "attach-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("同键重放应返回首次状态 201，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重放响应应带 Idempotency-Replayed 头")
	}

	// 同键不同负载 → 409。
	if code, body = doJSON(t, h, "POST", "/v1/attachments", "attach-1", "GUN-7", map[string]any{
		"code":             "CXLP-H-002",
		"product_batch_id": pbatchID,
		"client_event_id":  "evt-http-2",
		"occurred_at":      "2026-09-02T09:31:00+08:00",
	}); code != http.StatusConflict {
		t.Fatalf("同键不同负载应 409，得到 %d: %v", code, body)
	}

	// 采购商公开验证。
	code, body = doJSON(t, h, "GET", "/v1/verify/CXLP-H-001", "", "", nil)
	if code != http.StatusOK || body["valid"] != true {
		t.Fatalf("合规贴附应验证通过，%d: %v", code, body)
	}
	if body["spec"] != "1220x2440x18-E0" {
		t.Fatalf("验证应返回当前规格，得到 %v", body["spec"])
	}

	// 未贴附编号不暴露企业名。
	code, body = doJSON(t, h, "GET", "/v1/verify/CXLP-H-002", "", "", nil)
	if code != http.StatusOK || body["abnormal"] != true || body["producer_ref"] != nil {
		t.Fatalf("未贴附码应异常且无企业名，%d: %v", code, body)
	}

	// 挂失 002（企业保管期间）。
	if code, body = doJSON(t, h, "POST", "/v1/admin/codes/CXLP-H-002/lost", "lost-1", "MAKER-001", map[string]any{
		"reported_at": "2026-09-03T09:00:00+08:00",
		"note":        "箱件清点缺失",
	}); code != http.StatusAccepted {
		t.Fatalf("挂失 %d: %v", code, body)
	}

	// 品牌方整批遗失保管汇总：最后保管方应为生产企业。
	code, body = doJSON(t, h, "GET", "/v1/admin/print-batches/"+printBatchID+"/lost-custodians", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("保管汇总 %d: %v", code, body)
	}
	rows, _ := body["by_last_custodian"].([]any)
	if len(rows) != 1 {
		t.Fatalf("应只有企业一个保管方分组，得到 %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["custodian_type"] != "producer" || row["custodian_ref"] != "MAKER-001" {
		t.Fatalf("整批遗失最后保管方应为 MAKER-001，得到 %v", row)
	}

	// 单码追溯链完整。
	code, body = doJSON(t, h, "GET", "/v1/admin/codes/CXLP-H-002/trace", "", "", nil)
	if code != http.StatusOK || body["status"] != "lost" {
		t.Fatalf("追溯 %d: %v", code, body)
	}
	events, _ := body["events"].([]any)
	if len(events) < 5 {
		t.Fatalf("去向链应至少含 印制/装箱/交付/领用/挂失，实际 %d 条: %v", len(events), events)
	}
}

// TestHTTPBlockExpiredUnused 验证到期停码的 HTTP 路径。
func TestHTTPBlockExpiredUnused(t *testing.T) {
	h, cleanup := newRouter(t)
	defer cleanup()

	doJSON(t, h, "POST", "/v1/admin/attestations", "a1", "brand", map[string]any{
		"source_ref": "AUTH-X", "producer_ref": "MAKER-X", "product_ref": "P", "batch_ref": "b",
		"spec": "S", "inspection_digest": "d",
		"valid_until": "2026-08-31T23:59:59+08:00",
	})
	_, pb := doJSON(t, h, "POST", "/v1/admin/print-batches", "p1", "printer", map[string]any{
		"printer_ref": "PRINTER-A", "printed_at": "2026-07-01T09:00:00+08:00",
		"codes": []string{"CXLP-X-1"},
	})
	pbID, _ := pb["id"].(string)
	_, ct := doJSON(t, h, "POST", "/v1/admin/cartons", "c1", "printer", map[string]any{
		"print_batch_id": pbID, "codes": []string{"CXLP-X-1"},
	})
	ctID, _ := ct["id"].(string)
	doJSON(t, h, "POST", "/v1/admin/deliveries", "d1", "logistics", map[string]any{
		"producer_ref": "MAKER-X", "carton_ids": []string{ctID},
		"delivered_at": "2026-07-10T10:00:00+08:00",
	})

	code, body := doJSON(t, h, "POST", "/v1/admin/block-expired-unused", "blk-1", "brand", map[string]any{
		"now": "2026-09-05T00:00:00+08:00",
	})
	if code != http.StatusOK {
		t.Fatalf("停码 %d: %v", code, body)
	}
	if body["blocked"].(float64) != 1 {
		t.Fatalf("应停用 1 个，得到 %v", body["blocked"])
	}
	code, body = doJSON(t, h, "GET", "/v1/verify/CXLP-X-1", "", "", nil)
	if code != http.StatusOK || body["status"] != "blocked" {
		t.Fatalf("验证应为 blocked，%d: %v", code, body)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

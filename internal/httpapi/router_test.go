package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

func newTestRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.sqlite3"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewRouter(s), s
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return out
}

func TestHealth(t *testing.T) {
	h, _ := newTestRouter(t)
	rec := do(t, h, http.MethodGet, "/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("健康接口状态码为 %d", rec.Code)
	}
	if body := decodeBody(t, rec); body["status"] != "ok" {
		t.Fatalf("健康接口内容异常: %v", body)
	}
}

// 全链路：凭证→印批→装箱→交付→领用→离线贴附→公众验真。
func TestEndToEndFlow(t *testing.T) {
	h, _ := newTestRouter(t)
	must := func(name string, rec *httptest.ResponseRecorder, want int) map[string]any {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("%s 期望状态码 %d，实际 %d，body=%s", name, want, rec.Code, rec.Body.String())
		}
		return decodeBody(t, rec)
	}

	must("收录凭证", do(t, h, "POST", "/admin/authorizations", map[string]any{
		"credential_id":     "CRED-1",
		"producer_ref":      "MAKER-DEMO",
		"producer_name":     "曹县示范木业有限公司",
		"product_ref":       "SPEC-PLY-18",
		"spec_name":         "18mm 杉木生态板",
		"batch_ref":         "WOOD-DEMO",
		"inspection_digest": "sha256:demo",
		"valid_until":       "2026-12-31T23:59:59+08:00",
	}), http.StatusCreated)

	must("登记印批", do(t, h, "POST", "/admin/print-batches", map[string]any{
		"batch_id": "PB-1", "printer_ref": "PRESS-1", "printer_name": "曹县标签印厂",
		"product_ref": "SPEC-PLY-18", "spec_name": "18mm 杉木生态板",
		"printed_at": "2026-09-01T09:00:00+08:00",
		"codes":      []string{"L-1", "L-2"},
	}), http.StatusCreated)

	must("装箱", do(t, h, "POST", "/admin/boxes", map[string]any{
		"box_id": "BOX-1", "print_batch_id": "PB-1",
		"codes": []string{"L-1", "L-2"}, "packed_at": "2026-09-02T09:00:00+08:00",
	}), http.StatusCreated)

	must("交付", do(t, h, "POST", "/admin/boxes/BOX-1/deliver", map[string]any{
		"delivered_to_producer_ref":  "MAKER-DEMO",
		"delivered_to_producer_name": "曹县示范木业有限公司",
		"courier_ref":                "COURIER-1",
		"delivered_at":               "2026-09-03T09:00:00+08:00",
	}), http.StatusOK)

	must("领用", do(t, h, "POST", "/admin/labels/issue", map[string]any{
		"credential_id": "CRED-1", "codes": []string{"L-1"},
		"issued_at": "2026-09-05T09:00:00+08:00",
	}), http.StatusOK)

	applyBody := map[string]any{
		"device_id": "DEV-9", "device_event_id": "EVT-1001",
		"event_time": "2026-09-10T08:30:00+08:00",
		"items":      []map[string]any{{"code": "L-1", "product_batch_ref": "PLANK-BATCH-A"}},
	}
	must("首次贴附", do(t, h, "POST", "/devices/apply", applyBody), http.StatusAccepted)

	// 离线重传：整包原样再来一次，仍应成功且不产生新记录。
	body := must("重传贴附", do(t, h, "POST", "/devices/apply", applyBody), http.StatusAccepted)
	skipped, _ := body["skipped"].([]any)
	if len(skipped) != 1 {
		t.Fatalf("重传应跳过 1 个编号，实际 %v", body)
	}

	// 重传时改贴另一个批次：409。
	must("改贴批次", do(t, h, "POST", "/devices/apply", map[string]any{
		"device_id": "DEV-9", "device_event_id": "EVT-1001",
		"event_time": "2026-09-10T08:30:00+08:00",
		"items":      []map[string]any{{"code": "L-1", "product_batch_ref": "PLANK-BATCH-B"}},
	}), http.StatusConflict)

	// 公众验真。
	verify := must("公众验真", do(t, h, "GET", "/verify/L-1", nil), http.StatusOK)
	if verify["valid"] != true || verify["producer_name"] != "曹县示范木业有限公司" {
		t.Fatalf("验真内容异常: %v", verify)
	}
	if verify["product_batch_ref"] != "PLANK-BATCH-A" {
		t.Fatalf("验真应显示产品批次: %v", verify)
	}

	// 挂失后扫码须看到异常。
	must("挂失", do(t, h, "POST", "/admin/labels/lost", map[string]any{
		"codes": []string{"L-2"}, "actor_type": "producer", "actor_ref": "MAKER-DEMO",
		"actor_name": "曹县示范木业有限公司", "event_time": "2026-09-12T09:00:00+08:00",
	}), http.StatusOK)
	lostView := must("挂失验真", do(t, h, "GET", "/verify/L-2", nil), http.StatusOK)
	if lostView["valid"] != false || lostView["anomaly_code"] != "lost" {
		t.Fatalf("挂失编号应显示 lost 异常: %v", lostView)
	}

	// 品牌方溯源：整批最后保管人。
	trace := must("批次溯源", do(t, h, "GET", "/admin/print-batches/PB-1/trace", nil), http.StatusOK)
	lost, _ := trace["lost_labels"].([]any)
	if len(lost) != 1 {
		t.Fatalf("应列出 1 个挂失编号: %v", trace)
	}
	cust := lost[0].(map[string]any)["last_custodian"].(map[string]any)
	if cust["ref"] != "MAKER-DEMO" {
		t.Fatalf("最后保管人应为领用企业: %v", cust)
	}

	// 单码全链路台账。
	one := must("单码溯源", do(t, h, "GET", "/admin/labels/L-1/trace", nil), http.StatusOK)
	events, _ := one["events"].([]any)
	if len(events) != 5 { // printed/packed/delivered/issued/applied
		t.Fatalf("L-1 应有 5 条台账事件，实际 %d: %v", len(events), events)
	}

	// 未知编号 404；坏 JSON 400。
	must("未知编号", do(t, h, "GET", "/verify/NOPE", nil), http.StatusNotFound)
	req := httptest.NewRequest("POST", "/admin/boxes", bytes.NewBufferString("{bad json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，实际 %d", rec.Code)
	}
}

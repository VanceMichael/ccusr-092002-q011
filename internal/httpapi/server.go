// Package httpapi 暴露标签领用与核销的 HTTP 接口：
// /v1/verify/{code} 供采购商公开验证，/v1/admin 与 /v1 下的写入端供品牌方、印厂与离线设备使用。
package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vancemichael/092002-forest-brand-attest/internal/domain"
	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

type Server struct {
	store *store.Store
	svc   *domain.Service
}

func Router(st *store.Store, svc *domain.Service) http.Handler {
	s := &Server{store: st, svc: svc}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)

	// 采购商公开验证
	mux.HandleFunc("GET /v1/verify/{code}", s.verify)

	// 品牌方 / 印厂 / 企业写入端
	mux.Handle("POST /v1/admin/attestations", s.idempotent(http.HandlerFunc(s.importAttestation)))
	mux.Handle("POST /v1/admin/print-batches", s.idempotent(http.HandlerFunc(s.registerPrintBatch)))
	mux.Handle("POST /v1/admin/cartons", s.idempotent(http.HandlerFunc(s.packCarton)))
	mux.Handle("POST /v1/admin/deliveries", s.idempotent(http.HandlerFunc(s.createDelivery)))
	mux.Handle("POST /v1/admin/issues", s.idempotent(http.HandlerFunc(s.issueCodes)))
	mux.Handle("POST /v1/admin/product-batches", s.idempotent(http.HandlerFunc(s.registerProductBatch)))
	mux.Handle("POST /v1/attachments", s.idempotent(http.HandlerFunc(s.attachCode))) // 离线设备贴附
	mux.Handle("POST /v1/admin/codes/{code}/lost", s.idempotent(http.HandlerFunc(s.reportLost)))
	mux.Handle("POST /v1/admin/codes/{code}/recover", s.idempotent(http.HandlerFunc(s.recoverLost)))
	mux.Handle("POST /v1/admin/codes/{code}/void", s.idempotent(http.HandlerFunc(s.voidCode)))
	mux.Handle("POST /v1/admin/block-expired-unused", s.idempotent(http.HandlerFunc(s.blockExpiredUnused)))

	// 品牌方追溯
	mux.HandleFunc("GET /v1/admin/codes/{code}/trace", s.trace)
	mux.HandleFunc("GET /v1/admin/print-batches/{id}/lost-custodians", s.lostCustodians)

	return loggingFreeRecovery(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// 处理函数
// ---------------------------------------------------------------------------

func (s *Server) importAttestation(w http.ResponseWriter, r *http.Request) {
	var in domain.ImportAttestationInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.ImportAttestation(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) registerPrintBatch(w http.ResponseWriter, r *http.Request) {
	var in domain.RegisterPrintBatchInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.RegisterPrintBatch(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) packCarton(w http.ResponseWriter, r *http.Request) {
	var in domain.PackCartonInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.PackCarton(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) createDelivery(w http.ResponseWriter, r *http.Request) {
	var in domain.CreateDeliveryInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.CreateDelivery(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) issueCodes(w http.ResponseWriter, r *http.Request) {
	var in domain.IssueInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.IssueCodes(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) registerProductBatch(w http.ResponseWriter, r *http.Request) {
	var in domain.RegisterProductBatchInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.RegisterProductBatch(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) attachCode(w http.ResponseWriter, r *http.Request) {
	var in domain.AttachInput
	if !decode(w, r, &in) {
		return
	}
	out, err := s.svc.AttachCode(r.Context(), in, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

func (s *Server) reportLost(w http.ResponseWriter, r *http.Request) {
	var in domain.LostInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.svc.ReportLost(r.Context(), r.PathValue("code"), in, actor(r)); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"code": r.PathValue("code"), "status": "lost"})
}

func (s *Server) recoverLost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		At string `json:"at"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.svc.RecoverLost(r.Context(), r.PathValue("code"), body.At, actor(r)); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"code": r.PathValue("code"), "status": "recovered"})
}

func (s *Server) voidCode(w http.ResponseWriter, r *http.Request) {
	var in domain.VoidInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.svc.VoidCode(r.Context(), r.PathValue("code"), in, actor(r)); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"code": r.PathValue("code"), "status": "void"})
}

func (s *Server) blockExpiredUnused(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Now string `json:"now,omitempty"`
	}
	if !decode(w, r, &body) {
		return
	}
	now := s.store.Now()
	if body.Now != "" {
		t, err := time.Parse(time.RFC3339, body.Now)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("now 不是带时区偏移的 ISO 8601 时间"))
			return
		}
		now = t
	}
	n, err := s.svc.BlockExpiredUnused(r.Context(), now, actor(r))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": n, "evaluated_at": now.UTC().Format(time.RFC3339)})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Verify(r.Context(), r.PathValue("code"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) trace(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Trace(r.Context(), r.PathValue("code"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) lostCustodians(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.LostCustodians(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 幂等重放中间件（离线设备重传的第二道保险；贴附本身另有 client_event_id）
// ---------------------------------------------------------------------------

type recordingWriter struct {
	http.ResponseWriter
	buf     bytes.Buffer
	status  int
	written bool
}

func (r *recordingWriter) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recordingWriter) Write(b []byte) (int, error) {
	if !r.written {
		r.status = http.StatusOK
		r.written = true
	}
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// idempotent 依据 Idempotency-Key 头重放：无键时直通；同键不同负载拒绝；
// 处理中的并发重试返回 409；服务端故障时删除占位以允许重试。
func (s *Server) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("读取请求体失败"))
			return
		}
		hash := sha256.Sum256(body)
		hashHex := hex.EncodeToString(hash[:])
		r.Body = io.NopCloser(bytes.NewReader(body))

		ctx := r.Context()
		createdAt := s.store.Now().UTC().Format(time.RFC3339)

		// 先占用幂等键（status_code=0 表示处理中）。
		_, err = s.store.DB.ExecContext(ctx,
			`INSERT INTO idempotent_requests
			   (idempotency_key, method, path, request_hash, status_code, response_body, created_at)
			 VALUES (?,?,?,?,0,X'',?)`,
			key, r.Method, r.URL.Path, hashHex, createdAt)
		if err == nil {
			rec := &recordingWriter{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			finalStatus := rec.status
			if !rec.written {
				finalStatus = http.StatusOK
			}
			if finalStatus >= 500 {
				// 故障时释放占位，允许设备用同键重试。
				_, _ = s.store.DB.ExecContext(ctx,
					`DELETE FROM idempotent_requests WHERE idempotency_key=? AND status_code=0`, key)
				return
			}
			_, _ = s.store.DB.ExecContext(ctx,
				`UPDATE idempotent_requests SET status_code=?, response_body=? WHERE idempotency_key=?`,
				finalStatus, rec.buf.Bytes(), key)
			return
		}

		// 键已存在：必须同方法、同路径、同负载。
		var storedMethod, storedPath, storedHash string
		var storedStatus int
		var storedBody []byte
		err = s.store.DB.QueryRowContext(ctx,
			`SELECT method, path, request_hash, status_code, response_body
			 FROM idempotent_requests WHERE idempotency_key=?`, key).
			Scan(&storedMethod, &storedPath, &storedHash, &storedStatus, &storedBody)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("幂等记录读取失败"))
			return
		}
		if storedMethod != r.Method || storedPath != r.URL.Path {
			writeJSON(w, http.StatusConflict, errorBody("Idempotency-Key 已用于其它接口"))
			return
		}
		if storedHash != hashHex {
			writeJSON(w, http.StatusConflict, errorBody("Idempotency-Key 已用于不同的请求负载"))
			return
		}
		if storedStatus == 0 {
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusConflict, errorBody("相同 Idempotency-Key 的请求仍在处理中，请稍后重传"))
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(storedStatus)
		_, _ = w.Write(storedBody)
	})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func actor(r *http.Request) string {
	if a := strings.TrimSpace(r.Header.Get("X-Actor")); a != "" {
		return a
	}
	return "anonymous"
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("请求体不是合法 JSON 或含未知字段: "+err.Error()))
		return false
	}
	return true
}

func errorBody(message string) map[string]string {
	return map[string]string{"error": message}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeDomainError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrValidation):
		status = http.StatusBadRequest
	case errors.Is(err, domain.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, domain.ErrAttestExpired), errors.Is(err, domain.ErrAttestMismatch):
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, errorBody(err.Error()))
}

// loggingFreeRecovery 兜底，避免单次 panic 拖垮进程。
func loggingFreeRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeJSON(w, http.StatusInternalServerError,
					errorBody(fmt.Sprintf("内部错误: %v", rec)))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

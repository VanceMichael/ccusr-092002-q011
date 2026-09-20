// Package httpapi 暴露标签码领用与核销接口：管理端登记/领用/核销，设备端离线贴附，公众端验真，品牌方溯源。
package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/vancemichael/092002-forest-brand-attest/internal/store"
)

// Server 持有领域存储。
type Server struct {
	store *store.Store
}

// NewRouter 构建全部 HTTP 路由。
func NewRouter(s *store.Store) http.Handler {
	srv := &Server{store: s}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 授权凭证（只读外部依据）
	mux.HandleFunc("POST /admin/authorizations", srv.importAuthorization)

	// 印制批次与交付箱件
	mux.HandleFunc("POST /admin/print-batches", srv.registerPrintBatch)
	mux.HandleFunc("POST /admin/boxes", srv.packBox)
	mux.HandleFunc("POST /admin/boxes/{boxID}/deliver", srv.deliverBox)

	// 企业领用与核销
	mux.HandleFunc("POST /admin/labels/issue", srv.issueLabels)
	mux.HandleFunc("POST /admin/labels/lost", srv.reportLost)
	mux.HandleFunc("POST /admin/labels/recovered", srv.reportRecovered)
	mux.HandleFunc("POST /admin/labels/void", srv.voidLabels)
	mux.HandleFunc("POST /admin/maintenance/freeze-expired", srv.freezeExpired)

	// 离线设备贴附
	mux.HandleFunc("POST /devices/apply", srv.apply)

	// 公众验真与品牌方溯源
	mux.HandleFunc("GET /verify/{code}", srv.verify)
	mux.HandleFunc("GET /admin/labels/{code}/trace", srv.traceLabel)
	mux.HandleFunc("GET /admin/print-batches/{batchID}/trace", srv.traceBatch)

	return logRequests(mux)
}

func (s *Server) importAuthorization(w http.ResponseWriter, r *http.Request) {
	var in store.AuthorizationInput
	if !decode(w, r, &in) {
		return
	}
	created, err := s.store.ImportAuthorization(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"credential_id": in.CredentialID, "created": created})
}

func (s *Server) registerPrintBatch(w http.ResponseWriter, r *http.Request) {
	var in store.PrintBatchInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.RegisterPrintBatch(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) packBox(w http.ResponseWriter, r *http.Request) {
	var in store.BoxPackInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.PackBox(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) deliverBox(w http.ResponseWriter, r *http.Request) {
	var in store.BoxDeliverInput
	if !decode(w, r, &in) {
		return
	}
	in.BoxID = r.PathValue("boxID")
	res, err := s.store.DeliverBox(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) issueLabels(w http.ResponseWriter, r *http.Request) {
	var in store.IssueInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.Issue(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) reportLost(w http.ResponseWriter, r *http.Request) {
	var in store.ReportInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.ReportLost(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) reportRecovered(w http.ResponseWriter, r *http.Request) {
	var in store.ReportInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.ReportRecovered(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) voidLabels(w http.ResponseWriter, r *http.Request) {
	var in store.VoidInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.Void(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) freezeExpired(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.FreezeExpired(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var in store.ApplyInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.store.Apply(r.Context(), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.Verify(r.Context(), r.PathValue("code"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) traceLabel(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.TraceLabel(r.Context(), r.PathValue("code"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) traceBatch(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.TraceBatch(r.Context(), r.PathValue("batchID"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求体不是合法 JSON：" + err.Error(),
		})
		return false
	}
	return true
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		log.Printf("未预期错误: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "内部错误"})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

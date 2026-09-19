
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()
	Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("健康接口状态码为 %d", response.Code)
	}
}

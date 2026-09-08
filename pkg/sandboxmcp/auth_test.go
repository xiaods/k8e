package sandboxmcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/apikey"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type secretFixture struct {
	typedcore.SecretInterface
	records map[string]apikey.Record
	err     error
}

func (fixture *secretFixture) Get(context.Context, string, metav1.GetOptions) (*corev1.Secret, error) {
	if fixture.err != nil {
		return nil, fixture.err
	}
	data, err := apikey.Encode(fixture.records)
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "test-secret"}, Data: map[string][]byte{"keys.json": data}}, err
}

func TestAPIKeyLifecycle(t *testing.T) {
	record := apikey.NewRecord("original", 30, false, time.Now())
	fixture := &secretFixture{records: map[string]apikey.Record{"alice": record}}
	authenticate, err := NewAPIKeyAuthenticator(fixture, "sandbox-apikeys")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer original")
	first, err := authenticate(request)
	if err != nil || first.ID == "" {
		t.Fatalf("authentication: %v", err)
	}
	record.Key = "rotated"
	fixture.records["alice"] = record
	if _, err := authenticate(request); err == nil {
		t.Fatal("old key accepted")
	}
	request.Header.Set("Authorization", "Bearer rotated")
	rotated, err := authenticate(request)
	if err != nil || rotated != first {
		t.Fatal("rotation lost principal")
	}
	record.CreatedAt = record.CreatedAt.Add(time.Second)
	fixture.records["alice"] = record
	recreated, err := authenticate(request)
	if err != nil || recreated == first {
		t.Fatal("recreated key inherited owner")
	}
	expired := time.Now().Add(-time.Second)
	record.ExpiresAt = &expired
	fixture.records["alice"] = record
	if _, err := authenticate(request); err == nil {
		t.Fatal("expired key accepted")
	}
	delete(fixture.records, "alice")
	if _, err := authenticate(request); err == nil {
		t.Fatal("revoked key accepted")
	}
}

func TestAPIKeyHTTPFailures(t *testing.T) {
	fixture := &secretFixture{records: map[string]apikey.Record{"alice": {Key: "valid"}}}
	authenticate, _ := NewAPIKeyAuthenticator(fixture, "sandbox-apikeys")
	server, _ := New(Config{Authenticate: authenticate})
	for _, authorization := range []string{"", "Basic valid", "Bearer invalid"} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 401 || response.Header().Get("WWW-Authenticate") != `Bearer realm="k8e-mcp"` {
			t.Fatal(response)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp?api_key=valid", nil)
	request.Header.Set("Authorization", "Bearer valid")
	if _, err := authenticate(request); err == nil {
		t.Fatal("query credential accepted")
	}
	request = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Add("Authorization", "Bearer valid")
	request.Header.Add("Authorization", "Bearer valid")
	if _, err := authenticate(request); err == nil {
		t.Fatal("duplicate auth accepted")
	}
	request.Header.Set("Authorization", "Bearer valid")
	fixture.records["bob"] = apikey.Record{Key: "valid"}
	if _, err := authenticate(request); err == nil {
		t.Fatal("ambiguous identity accepted")
	}
	fixture.err = errors.New("private storage error")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatal(response)
	}
}

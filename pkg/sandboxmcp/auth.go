package sandboxmcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/apikey"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type authenticationError struct {
	status    int
	challenge string
}

func (err *authenticationError) Error() string { return "API key rejected" }

func NewAPIKeyAuthenticator(secrets typedcore.SecretInterface, secretName string) (Authenticate, error) {
	if secrets == nil || secretName == "" {
		return nil, errors.New("API key Secret client and name required")
	}
	return func(request *http.Request) (Principal, error) {
		denied := &authenticationError{status: http.StatusUnauthorized, challenge: `Bearer realm="k8e-mcp"`}
		key, ok := bearerAPIKey(request)
		if !ok {
			return Principal{}, denied
		}
		secret, err := secrets.Get(request.Context(), secretName, metav1.GetOptions{})
		if err != nil {
			return Principal{}, &authenticationError{status: http.StatusServiceUnavailable}
		}
		records, err := apikey.Parse(secret.Data["keys.json"])
		if err != nil {
			return Principal{}, &authenticationError{status: http.StatusServiceUnavailable}
		}
		principal, ok := matchAPIKey(key, string(secret.UID), records, time.Now())
		if !ok {
			return Principal{}, denied
		}
		return principal, nil
	}, nil
}

func bearerAPIKey(request *http.Request) (string, bool) {
	headers := request.Header.Values("Authorization")
	if len(headers) != 1 || request.URL.Query().Has("access_token") || request.URL.Query().Has("api_key") {
		return "", false
	}
	parts := strings.Fields(headers[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 4096 {
		return "", false
	}
	return parts[1], true
}

func matchAPIKey(key, secretUID string, records map[string]apikey.Record, now time.Time) (Principal, bool) {
	candidate := sha256.Sum256([]byte(key))
	principal := Principal{}
	matches := 0
	for name, record := range records {
		digest := sha256.Sum256([]byte(record.Key))
		matched := subtle.ConstantTimeCompare(candidate[:], digest[:]) == 1
		if matched && name != "" && record.Key != "" && !record.Expired(now) {
			matches++
			principal.ID = recordKey("apikey", secretUID, name, record.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	return principal, matches == 1
}

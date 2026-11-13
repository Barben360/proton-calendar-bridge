package auth

import (
	"net/http"

	gpa "github.com/ProtonMail/go-proton-api"
)

// NewManagerOver is NewManager on transport t (a test server's TLS).
func NewManagerOver(baseURL string, t *http.Transport) *gpa.Manager {
	return newManager(baseURL, "", t)
}

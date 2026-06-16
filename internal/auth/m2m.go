package auth

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// --- Request/Response types ---

type createM2MRequest struct {
	TenantID string   `json:"tenantId"`
	UserID   string   `json:"userId"`
	Roles    []string `json:"roles"`
}

type m2mClientResponse struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

type m2mClientInfoResponse struct {
	ClientID string   `json:"clientId"`
	TenantID string   `json:"tenantId"`
	UserID   string   `json:"userId"`
	Roles    []string `json:"roles"`
}

// --- M2MHandler ---

// M2MHandler handles HTTP requests for M2M client management.
type M2MHandler struct {
	m2mStore M2MClientStore
}

// NewM2MHandler creates a new M2MHandler.
func NewM2MHandler(store M2MClientStore) *M2MHandler {
	return &M2MHandler{m2mStore: store}
}

// ServeHTTP routes M2M client management requests. Requires ROLE_ADMIN.
func (h *M2MHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !RequireAdmin(w, r) {
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")

	// POST /account/m2m/{clientId}/secret/reset
	if r.Method == http.MethodPost && strings.HasSuffix(path, "/secret/reset") {
		clientID := extractM2MClientID(path)
		if clientID == "" {
			http.Error(w, `{"error":"missing clientId"}`, http.StatusBadRequest)
			return
		}
		h.handleResetSecret(w, clientID)
		return
	}

	// Exact match: /account/m2m
	if path == "/account/m2m" {
		switch r.Method {
		case http.MethodGet:
			h.handleList(w)
		case http.MethodPost:
			h.handleCreate(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}

	// DELETE /account/m2m/{clientId}
	segments := strings.Split(path, "/")
	// Expected: ["", "account", "m2m", "{clientId}"]
	if len(segments) == 4 && segments[1] == "account" && segments[2] == "m2m" && segments[3] != "" {
		if r.Method == http.MethodDelete {
			h.handleDelete(w, segments[3])
			return
		}
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func (h *M2MHandler) handleList(w http.ResponseWriter) {
	clients := h.m2mStore.List()
	resp := make([]m2mClientInfoResponse, 0, len(clients))
	for _, c := range clients {
		resp = append(resp, m2mClientInfoResponse{
			ClientID: c.ClientID,
			TenantID: string(c.TenantID),
			UserID:   c.UserID,
			Roles:    c.Roles,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *M2MHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	// Limit request body to 1MB to prevent abuse.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req createM2MRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.TenantID == "" || req.UserID == "" {
		http.Error(w, `{"error":"tenantId and userId are required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Roles) > 100 {
		http.Error(w, `{"error":"too many roles (max 100)"}`, http.StatusBadRequest)
		return
	}

	clientID := uuid.NewString()
	secret, err := h.m2mStore.Create(clientID, spi.TenantID(req.TenantID), req.UserID, req.Roles)
	if err != nil {
		http.Error(w, `{"error":"failed to create m2m client"}`, http.StatusInternalServerError)
		return
	}

	resp := m2mClientResponse{
		ClientID:     clientID,
		ClientSecret: secret,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *M2MHandler) handleDelete(w http.ResponseWriter, clientID string) {
	if err := h.m2mStore.Delete(clientID); err != nil {
		http.Error(w, `{"error":"m2m client not found"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *M2MHandler) handleResetSecret(w http.ResponseWriter, clientID string) {
	newSecret, err := h.m2mStore.ResetSecret(clientID)
	if err != nil {
		http.Error(w, `{"error":"m2m client not found"}`, http.StatusNotFound)
		return
	}

	resp := m2mClientResponse{
		ClientID:     clientID,
		ClientSecret: newSecret,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// extractM2MClientID extracts the clientId from a path like /account/m2m/{clientId}/secret/reset.
func extractM2MClientID(path string) string {
	segments := strings.Split(path, "/")
	// Expected: ["", "account", "m2m", "{clientId}", "secret", "reset"]
	if len(segments) == 6 && segments[1] == "account" && segments[2] == "m2m" && segments[4] == "secret" && segments[5] == "reset" {
		return segments[3]
	}
	return ""
}

package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mchenetz/pxobj/internal/cluster"
	"github.com/mchenetz/pxobj/internal/objectd"
)

type Handler struct {
	Store   *objectd.Store
	Token   string
	Cluster *cluster.Cluster
}

func New(store *objectd.Store, token string, c *cluster.Cluster) *Handler {
	return &Handler{Store: store, Token: token, Cluster: c}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.shouldProxyToLeader(r) {
		if err := h.Cluster.ProxyToLeader(w, r, "admin"); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/admin/buckets" {
		h.createBucket(w, r)
		return
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/admin/buckets/") {
		h.deleteBucket(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/admin/access" {
		h.createAccess(w, r)
		return
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/admin/access/") {
		h.deleteAccess(w, r)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) shouldProxyToLeader(r *http.Request) bool {
	if h.Cluster == nil || !h.Cluster.Enabled() || h.Cluster.IsInternalReplication(r) {
		return false
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		return false
	}
	return !h.Cluster.IsLeader(r.Context())
}

func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := h.Store.CreateBucket(r.Context(), req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodPost, "/_cluster/replicate/buckets/"+req.Name, nil, nil); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) deleteBucket(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/buckets/")
	if name == "" {
		http.Error(w, "missing bucket", http.StatusBadRequest)
		return
	}
	if err := h.Store.DeleteBucket(r.Context(), name); err != nil {
		if errors.Is(err, objectd.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodDelete, "/_cluster/replicate/buckets/"+name, nil, nil); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bucket   string `json:"bucket"`
		ReadOnly bool   `json:"readOnly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Bucket == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ak, err := h.Store.CreateAccess(r.Context(), req.Bucket, req.ReadOnly)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		payload, _ := json.Marshal(ak)
		if err := h.Cluster.Replicate(r.Context(), http.MethodPost, "/_cluster/replicate/access", map[string]string{"Content-Type": "application/json"}, payload); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ak)
}

func (h *Handler) deleteAccess(w http.ResponseWriter, r *http.Request) {
	accessKey := strings.TrimPrefix(r.URL.Path, "/admin/access/")
	if accessKey == "" {
		http.Error(w, "missing access key", http.StatusBadRequest)
		return
	}
	if err := h.Store.DeleteAccess(r.Context(), accessKey); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodDelete, "/_cluster/replicate/access/"+accessKey, nil, nil); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

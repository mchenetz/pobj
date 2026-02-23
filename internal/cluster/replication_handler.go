package cluster

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mchenetz/pxobj/internal/objectd"
)

type ReplicationHandler struct {
	Store *objectd.Store
	Token string
}

func NewReplicationHandler(store *objectd.Store, token string) *ReplicationHandler {
	return &ReplicationHandler{Store: store, Token: token}
}

func (h *ReplicationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/_cluster/health" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if r.Header.Get("X-PXOBJ-Internal-Replication") != "true" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !hasPeerClientCert(r) {
		http.Error(w, "mTLS required", http.StatusForbidden)
		return
	}

	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/_cluster/replicate/buckets/"):
		name := strings.TrimPrefix(r.URL.Path, "/_cluster/replicate/buckets/")
		if err := h.Store.CreateBucket(r.Context(), name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/_cluster/replicate/buckets/"):
		name := strings.TrimPrefix(r.URL.Path, "/_cluster/replicate/buckets/")
		if err := h.Store.DeleteBucket(r.Context(), name); err != nil && err != objectd.ErrNotFound {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/_cluster/replicate/objects/"):
		rest := strings.TrimPrefix(r.URL.Path, "/_cluster/replicate/objects/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if _, err := h.Store.PutObject(r.Context(), parts[0], parts[1], r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/_cluster/replicate/objects/"):
		rest := strings.TrimPrefix(r.URL.Path, "/_cluster/replicate/objects/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if err := h.Store.DeleteObject(r.Context(), parts[0], parts[1]); err != nil && err != objectd.ErrNotFound {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/_cluster/replicate/access":
		var a objectd.AccessKey
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := h.Store.PutAccess(r.Context(), a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/_cluster/replicate/access/"):
		ak := strings.TrimPrefix(r.URL.Path, "/_cluster/replicate/access/")
		if err := h.Store.DeleteAccess(r.Context(), ak); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func hasPeerClientCert(r *http.Request) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	if r.TLS.VerifiedChains == nil || len(r.TLS.VerifiedChains) == 0 {
		return false
	}
	leaf := r.TLS.PeerCertificates[0]
	return leaf.ExtKeyUsage == nil || hasClientAuthUsage(leaf)
}

func hasClientAuthUsage(cert *x509.Certificate) bool {
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

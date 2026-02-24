package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mchenetz/entity/internal/cluster"
	"github.com/mchenetz/entity/internal/objectd"
)

type Resolver struct{ Store *objectd.Store }

func (r Resolver) Lookup(accessKey string) (secret string, bucket string, readOnly bool, err error) {
	a, err := r.Store.LookupAccessKey(context.Background(), accessKey)
	if err != nil {
		return "", "", false, err
	}
	return a.SecretKey, a.Bucket, a.ReadOnly, nil
}

type Handler struct {
	Store    *objectd.Store
	Resolver Resolver
	Cluster  *cluster.Cluster
}

func NewHandler(s *objectd.Store, c *cluster.Cluster) *Handler {
	return &Handler{Store: s, Resolver: Resolver{Store: s}, Cluster: c}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := VerifySigV4(r, h.Resolver)
	if err != nil {
		writeError(w, "AccessDenied", err.Error(), http.StatusForbidden)
		return
	}
	bucket, key := splitPath(r.URL.Path)

	if bucket != "" && auth.Bucket != bucket {
		writeError(w, "AccessDenied", "bucket not allowed", http.StatusForbidden)
		return
	}
	if auth.ReadOnly && (r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		writeError(w, "AccessDenied", "read-only credentials", http.StatusForbidden)
		return
	}

	if h.shouldProxyToLeader(r, bucket, key) {
		if err := h.Cluster.ProxyToLeader(w, r, "s3"); err != nil {
			writeError(w, "InternalError", err.Error(), http.StatusServiceUnavailable)
		}
		return
	}

	switch {
	case r.Method == http.MethodGet && bucket == "" && key == "":
		h.listBuckets(w, r, auth.Bucket)
	case r.Method == http.MethodPut && bucket != "" && key == "":
		h.createBucket(w, r, bucket)
	case r.Method == http.MethodDelete && bucket != "" && key == "":
		h.deleteBucket(w, r, bucket)
	case r.Method == http.MethodGet && bucket != "" && key == "" && r.URL.Query().Get("list-type") == "2":
		h.listObjectsV2(w, r, bucket)
	case r.Method == http.MethodPut && bucket != "" && key != "":
		h.putObject(w, r, bucket, key)
	case r.Method == http.MethodGet && bucket != "" && key != "":
		h.getObject(w, r, bucket, key)
	case r.Method == http.MethodHead && bucket != "" && key != "":
		h.headObject(w, r, bucket, key)
	case r.Method == http.MethodDelete && bucket != "" && key != "":
		h.deleteObject(w, r, bucket, key)
	default:
		writeError(w, "NotImplemented", "operation not implemented", http.StatusNotImplemented)
	}
}

func (h *Handler) shouldProxyToLeader(r *http.Request, bucket, key string) bool {
	if h.Cluster == nil || !h.Cluster.Enabled() || h.Cluster.IsInternalReplication(r) {
		return false
	}
	if !isMutatingS3(r.Method, bucket, key) {
		return false
	}
	return !h.Cluster.IsLeader(r.Context())
}

func isMutatingS3(method, bucket, key string) bool {
	if method == http.MethodPut && bucket != "" {
		return true
	}
	if method == http.MethodDelete && bucket != "" {
		return true
	}
	return false
}

func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request, allowedBucket string) {
	buckets, err := h.Store.ListBuckets(r.Context())
	if err != nil {
		writeError(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}
	type bucketEntry struct {
		Name         string `xml:"Name"`
		CreationDate string `xml:"CreationDate"`
	}
	resp := struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Xmlns   string   `xml:"xmlns,attr"`
		Buckets struct {
			Bucket []bucketEntry `xml:"Bucket"`
		} `xml:"Buckets"`
	}{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, b := range buckets {
		if allowedBucket != "" && b.Name != allowedBucket {
			continue
		}
		resp.Buckets.Bucket = append(resp.Buckets.Bucket, bucketEntry{Name: b.Name, CreationDate: b.CreatedAt.Format(time.RFC3339)})
	}
	writeXML(w, http.StatusOK, resp)
}

func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.Store.CreateBucket(r.Context(), bucket); err != nil {
		writeError(w, "InvalidBucketName", err.Error(), http.StatusBadRequest)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodPost, "/_cluster/replicate/buckets/"+bucket, nil, nil); err != nil {
			writeError(w, "InternalError", err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.Store.DeleteBucket(r.Context(), bucket); err != nil {
		if errors.Is(err, objectd.ErrNotFound) {
			writeError(w, "NoSuchBucket", "bucket does not exist", http.StatusNotFound)
			return
		}
		writeError(w, "BucketNotEmpty", err.Error(), http.StatusConflict)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodDelete, "/_cluster/replicate/buckets/"+bucket, nil, nil); err != nil {
			writeError(w, "InternalError", err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	token := q.Get("continuation-token")
	maxKeys := 1000
	if mk := q.Get("max-keys"); mk != "" {
		if v, err := strconv.Atoi(mk); err == nil {
			maxKeys = v
		}
	}
	objects, next, truncated, err := h.Store.ListObjectsV2(r.Context(), bucket, prefix, token, maxKeys)
	if err != nil {
		writeError(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}
	type contents struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	resp := struct {
		XMLName               xml.Name   `xml:"ListBucketResult"`
		Xmlns                 string     `xml:"xmlns,attr"`
		Name                  string     `xml:"Name"`
		Prefix                string     `xml:"Prefix"`
		MaxKeys               int        `xml:"MaxKeys"`
		IsTruncated           bool       `xml:"IsTruncated"`
		NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
		Contents              []contents `xml:"Contents"`
	}{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket,
		Prefix:                prefix,
		MaxKeys:               maxKeys,
		IsTruncated:           truncated,
		NextContinuationToken: next,
	}
	for _, o := range objects {
		resp.Contents = append(resp.Contents, contents{Key: o.Key, LastModified: o.ModTime.Format(time.RFC3339), ETag: fmt.Sprintf("\"%s\"", o.ETag), Size: o.Size, StorageClass: "STANDARD"})
	}
	writeXML(w, http.StatusOK, resp)
}

func (h *Handler) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, "InternalError", err.Error(), http.StatusBadRequest)
		return
	}
	obj, err := h.Store.PutObject(r.Context(), bucket, key, bytes.NewReader(payload))
	if err != nil {
		if errors.Is(err, objectd.ErrNotFound) {
			writeError(w, "NoSuchBucket", err.Error(), http.StatusNotFound)
			return
		}
		writeError(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodPut, "/_cluster/replicate/objects/"+bucket+"/"+key, map[string]string{"Content-Type": "application/octet-stream"}, payload); err != nil {
			writeError(w, "InternalError", err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", obj.ETag))
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	meta, f, err := h.Store.OpenObject(r.Context(), bucket, key)
	if err != nil {
		if errors.Is(err, objectd.ErrNotFound) {
			writeError(w, "NoSuchKey", "object not found", http.StatusNotFound)
			return
		}
		writeError(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Last-Modified", meta.ModTime.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (h *Handler) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	meta, err := h.Store.GetObjectMeta(r.Context(), bucket, key)
	if err != nil {
		if errors.Is(err, objectd.ErrNotFound) {
			writeError(w, "NoSuchKey", "object not found", http.StatusNotFound)
			return
		}
		writeError(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", meta.ETag))
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Last-Modified", meta.ModTime.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if err := h.Store.DeleteObject(r.Context(), bucket, key); err != nil && !errors.Is(err, objectd.ErrNotFound) {
		writeError(w, "InternalError", err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Cluster != nil && h.Cluster.Enabled() {
		if err := h.Cluster.Replicate(r.Context(), http.MethodDelete, "/_cluster/replicate/objects/"+bucket+"/"+key, nil, nil); err != nil {
			writeError(w, "InternalError", err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	parts := strings.SplitN(p, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func writeXML(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code, msg string, status int) {
	type errResp struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	writeXML(w, status, errResp{Code: code, Message: msg})
}

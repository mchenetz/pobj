package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	PodName      string
	Namespace    string
	Name         string
	HeadlessName string
	Replicas     int
	S3Port       int
	AdminPort    int
	Token        string

	TLSEnabled bool
	CAFile     string
	CertFile   string
	KeyFile    string
}

type Cluster struct {
	cfg        Config
	ordinal    int
	httpClient *http.Client
}

func New(cfg Config) *Cluster {
	if cfg.Replicas <= 0 {
		cfg.Replicas = 1
	}
	if cfg.S3Port == 0 {
		cfg.S3Port = 9000
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = 19000
	}
	tr := &http.Transport{}
	if cfg.TLSEnabled {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			if b, err := os.ReadFile(cfg.CAFile); err == nil {
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(b)
				tlsCfg.RootCAs = pool
			}
		}
		if cfg.CertFile != "" && cfg.KeyFile != "" {
			if cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err == nil {
				tlsCfg.Certificates = []tls.Certificate{cert}
			}
		}
		tr.TLSClientConfig = tlsCfg
	}
	return &Cluster{
		cfg:        cfg,
		ordinal:    parseOrdinal(cfg.PodName),
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}
}

func (c *Cluster) Enabled() bool    { return c.cfg.Replicas > 1 }
func (c *Cluster) SelfOrdinal() int { return c.ordinal }

func (c *Cluster) IsInternalReplication(r *http.Request) bool {
	return r.Header.Get("X-PXOBJ-Internal-Replication") == "true"
}

func (c *Cluster) Leader(ctx context.Context) (int, string) {
	if !c.Enabled() {
		return 0, c.adminURL(0)
	}
	for i := 0; i < c.cfg.Replicas; i++ {
		if c.health(ctx, i) {
			return i, c.adminURL(i)
		}
	}
	return 0, c.adminURL(0)
}

func (c *Cluster) IsLeader(ctx context.Context) bool {
	l, _ := c.Leader(ctx)
	return l == c.ordinal
}

func (c *Cluster) ProxyToLeader(w http.ResponseWriter, r *http.Request, service string) error {
	_, admin := c.Leader(r.Context())
	base := admin
	if service == "s3" {
		base = strings.Replace(admin, fmt.Sprintf(":%d", c.cfg.AdminPort), fmt.Sprintf(":%d", c.cfg.S3Port), 1)
	}
	url := base + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		return err
	}
	req.Header = r.Header.Clone()
	req.Host = r.Host
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

func (c *Cluster) Replicate(ctx context.Context, method, path string, headers map[string]string, body []byte) error {
	if !c.Enabled() {
		return nil
	}
	acks := 1
	required := (c.cfg.Replicas / 2) + 1
	for i := 0; i < c.cfg.Replicas; i++ {
		if i == c.ordinal {
			continue
		}
		url := c.adminURL(i) + path
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		req.Header.Set("X-PXOBJ-Internal-Replication", "true")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			acks++
		}
	}
	if acks < required {
		return fmt.Errorf("replication quorum not reached: got=%d required=%d", acks, required)
	}
	return nil
}

func (c *Cluster) health(ctx context.Context, ordinal int) bool {
	url := c.adminURL(ordinal) + "/_cluster/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *Cluster) adminURL(ordinal int) string {
	host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", c.cfg.Name, ordinal, c.cfg.HeadlessName, c.cfg.Namespace)
	scheme := "http"
	if c.cfg.TLSEnabled {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, c.cfg.AdminPort)
}

func parseOrdinal(podName string) int {
	parts := strings.Split(podName, "-")
	if len(parts) == 0 {
		return 0
	}
	v, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return v
}

func copyHeader(dst, src http.Header) {
	for k, vals := range src {
		dst.Del(k)
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

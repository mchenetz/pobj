package cosi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type AdminClient struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

type AccessKey struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	Bucket    string `json:"bucket"`
	ReadOnly  bool   `json:"readOnly"`
}

func NewAdminClient(baseURL, token, caPEM string) *AdminClient {
	tr := &http.Transport{}
	if caPEM != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(caPEM))
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &AdminClient{BaseURL: baseURL, Token: token, Client: &http.Client{Timeout: 15 * time.Second, Transport: tr}}
}

func (c *AdminClient) CreateBucket(ctx context.Context, name string) error {
	payload, _ := json.Marshal(map[string]string{"name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/buckets", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("create bucket failed: %s", resp.Status)
	}
	return nil
}

func (c *AdminClient) DeleteBucket(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/admin/buckets/"+name, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		return fmt.Errorf("delete bucket failed: %s", resp.Status)
	}
	return nil
}

func (c *AdminClient) CreateAccess(ctx context.Context, bucket string, readOnly bool) (AccessKey, error) {
	payload, _ := json.Marshal(map[string]any{"bucket": bucket, "readOnly": readOnly})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/access", bytes.NewReader(payload))
	if err != nil {
		return AccessKey{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return AccessKey{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return AccessKey{}, fmt.Errorf("create access failed: %s", resp.Status)
	}
	var out AccessKey
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AccessKey{}, err
	}
	return out, nil
}

func (c *AdminClient) DeleteAccess(ctx context.Context, accessKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/admin/access/"+accessKey, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		return fmt.Errorf("delete access failed: %s", resp.Status)
	}
	return nil
}

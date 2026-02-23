package main

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mchenetz/pxobj/internal/admin"
	"github.com/mchenetz/pxobj/internal/cluster"
	"github.com/mchenetz/pxobj/internal/objectd"
	"github.com/mchenetz/pxobj/internal/s3"
)

func main() {
	dataDir := getEnv("PXOBJ_DATA_DIR", "/data")
	s3Port := getEnv("PXOBJ_S3_PORT", "9000")
	adminPort := getEnv("PXOBJ_ADMIN_PORT", "19000")
	adminToken := os.Getenv("PXOBJ_ADMIN_TOKEN")
	if adminToken == "" {
		log.Fatal("PXOBJ_ADMIN_TOKEN must be set")
	}
	tlsEnabled := strings.EqualFold(getEnv("PXOBJ_TLS_ENABLED", "false"), "true")
	certFile := os.Getenv("PXOBJ_TLS_CERT_FILE")
	keyFile := os.Getenv("PXOBJ_TLS_KEY_FILE")
	caFile := os.Getenv("PXOBJ_TLS_CA_FILE")

	clusterCfg := cluster.Config{
		PodName:      os.Getenv("POD_NAME"),
		Namespace:    getEnv("POD_NAMESPACE", "default"),
		Name:         getEnv("PXOBJ_SERVICE_NAME", "pxobj"),
		HeadlessName: getEnv("PXOBJ_HEADLESS_SERVICE_NAME", "pxobj-headless"),
		Replicas:     atoiDefault(os.Getenv("PXOBJ_REPLICAS"), 1),
		S3Port:       atoiDefault(s3Port, 9000),
		AdminPort:    atoiDefault(adminPort, 19000),
		Token:        adminToken,
		TLSEnabled:   tlsEnabled,
		CAFile:       caFile,
		CertFile:     certFile,
		KeyFile:      keyFile,
	}
	if clusterCfg.PodName == "" {
		clusterCfg.PodName = clusterCfg.Name + "-0"
	}
	cl := cluster.New(clusterCfg)

	store, err := objectd.OpenStore(dataDir)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	s3Mux := http.NewServeMux()
	s3Mux.Handle("/", s3.NewHandler(store, cl))
	adminMux := http.NewServeMux()
	adminMux.Handle("/_cluster/", cluster.NewReplicationHandler(store, adminToken))
	adminMux.Handle("/admin/", admin.New(store, adminToken, cl))

	s3Srv := &http.Server{
		Addr:              ":" + s3Port,
		Handler:           s3Mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              ":" + adminPort,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if tlsEnabled {
		tlsCfg, err := makeServerTLSConfig(certFile, keyFile, caFile)
		if err != nil {
			log.Fatalf("failed to build TLS config: %v", err)
		}
		s3Srv.TLSConfig = tlsCfg.Clone()
		adminTLS := tlsCfg.Clone()
		adminTLS.ClientAuth = tls.VerifyClientCertIfGiven
		adminSrv.TLSConfig = adminTLS
	}

	go func() {
		log.Printf("S3 API listening on %s", s3Srv.Addr)
		var err error
		if tlsEnabled {
			err = s3Srv.ListenAndServeTLS("", "")
		} else {
			err = s3Srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("s3 server error: %v", err)
		}
	}()
	go func() {
		log.Printf("Admin API listening on %s", adminSrv.Addr)
		var err error
		if tlsEnabled {
			err = adminSrv.ListenAndServeTLS("", "")
		} else {
			err = adminSrv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("admin server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	_ = s3Srv.Close()
	_ = adminSrv.Close()
}

func makeServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if caFile != "" {
		caBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, err
		}
		tlsCfg.ClientCAs = pool
	}
	return tlsCfg, nil
}

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func atoiDefault(v string, d int) int {
	i, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return d
	}
	return i
}

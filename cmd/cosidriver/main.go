package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mchenetz/pxobj/internal/cosi"
	cosictrl "sigs.k8s.io/container-object-storage-interface-api/controller"
)

func main() {
	var identity string
	var lockName string
	var threads int
	flag.StringVar(&identity, "identity", os.Getenv("POD_NAME"), "leader identity")
	flag.StringVar(&lockName, "leader-lock", "pxobj-cosi", "leader lock name")
	flag.IntVar(&threads, "threads", 4, "worker threads")
	flag.Parse()

	driverName := env("PXOBJ_DRIVER_NAME", "pxobj.io/s3")
	endpoint := env("PXOBJ_S3_ENDPOINT", "pxobj.default.svc.cluster.local:9000")
	region := env("PXOBJ_S3_REGION", "us-east-1")
	s3CAPEM := os.Getenv("PXOBJ_S3_CA_PEM")
	adminURL := env("PXOBJ_ADMIN_URL", "https://pxobj.default.svc.cluster.local:19000")
	adminCAPEM := os.Getenv("PXOBJ_ADMIN_CA_PEM")
	adminToken := os.Getenv("PXOBJ_ADMIN_TOKEN")
	if adminToken == "" {
		log.Fatal("PXOBJ_ADMIN_TOKEN is required")
	}

	admin := cosi.NewAdminClient(adminURL, adminToken, adminCAPEM)
	listener := cosi.NewListener(driverName, endpoint, region, s3CAPEM, admin)

	ctrl, err := cosictrl.NewDefaultObjectStorageController(identity, lockName, threads)
	if err != nil {
		log.Fatalf("failed to create COSI controller: %v", err)
	}

	ctrl.AddBucketListener(listener)
	ctrl.AddBucketAccessListener(cosi.BucketAccessListenerAdapter{Listener: listener})
	ctrl.AddBucketClassListener(cosi.NoopBucketClassListener{})
	ctrl.AddBucketAccessClassListener(cosi.NoopBucketAccessClassListener{})
	ctrl.AddBucketClaimListener(cosi.BucketClaimListenerAdapter{Listener: listener})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()
	if err := ctrl.Run(ctx); err != nil {
		log.Fatalf("controller error: %v", err)
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

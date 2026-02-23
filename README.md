# pobj

`pxobj` is a full custom COSI-compliant S3 object service for Kubernetes, implemented without MinIO.

Full user documentation (installation, order of operations, TLS modes, scaling, examples):
- [docs/USER_GUIDE.md](docs/USER_GUIDE.md)

CI/release workflow:
- [build-release.yml](.github/workflows/build-release.yml)

Helm chart:
- [charts/pxobj](charts/pxobj)

Prebuilt Helm chart (GHCR OCI):
- `oci://ghcr.io/mchenetz/charts/pxobj`

## What this includes

- Custom object server (`cmd/objectd`) with:
  - S3-compatible API (`ListBuckets`, `Create/DeleteBucket`, `ListObjectsV2`, `Put/Get/Head/DeleteObject`)
  - AWS Signature V4 authentication
  - Per-bucket scoped access keys and read-only/read-write policies
- Admin API used by the COSI backend listener
- Operator (`cmd/operator`) with `ObjectService` CRD that:
  - Provisions/updates a Portworx-backed StatefulSet
  - Creates service and admin token secret
  - Reconciles an in-cluster COSI controller deployment
- COSI controller (`cmd/cosidriver`) using the upstream COSI API listeners:
  - Provisions Bucket resources in backend
  - Grants/revokes BucketAccess credentials
  - Writes workload credentials Secret
- Clustered object layer:
  - StatefulSet peer discovery via headless service
  - Leader routing for mutating S3/Admin operations
  - Synchronous follower replication with quorum checks
- TLS and cert handling:
  - Auto-generated and rotated self-signed certs (default)
  - Optional cert-manager Certificate reconciliation
  - HTTPS for S3/Admin APIs
  - mTLS enforcement on intra-cluster replication endpoints

## Usage

Set `spec.storageClassName` in `ObjectService` to your StorageClass (example: `px-repl3`).
Every object server pod uses a dedicated PVC from that class.

## Build

```bash
go build ./...
make docker-build IMAGE=ghcr.io/mchenetz/pxobj:latest
```

## Deploy

```bash
make deploy
```

This applies:
- `ObjectService` CRD
- COSI CRDs
- RBAC/ServiceAccounts
- Operator deployment
- Sample `ObjectService`
- Sample COSI classes

## Install Prebuilt Helm Chart From GHCR

Login to GHCR (Helm OCI):

```bash
echo <GITHUB_TOKEN> | helm registry login ghcr.io -u <GITHUB_USERNAME> --password-stdin
```

Install directly from registry:

```bash
helm upgrade --install pxobj oci://ghcr.io/mchenetz/charts/pxobj \
  --version 0.1.0 \
  --namespace pxobj-system --create-namespace
```

Install and create ObjectService + COSI classes:

```bash
helm upgrade --install pxobj oci://ghcr.io/mchenetz/charts/pxobj \
  --version 0.1.0 \
  --namespace pxobj-system --create-namespace \
  --set image.repository=ghcr.io/mchenetz/pxobj \
  --set image.tag=latest \
  --set objectService.create=true \
  --set objectService.storageClassName=px-repl3 \
  --set cosi.createClasses=true
```

Optional: pull chart locally first:

```bash
helm pull oci://ghcr.io/mchenetz/charts/pxobj --version 0.1.0
tar -xzf pxobj-0.1.0.tgz
helm upgrade --install pxobj ./pxobj --namespace pxobj-system --create-namespace
```

## End-to-end test on Kind

Run the full Kind e2e (build image, deploy stack, create COSI bucket/access, perform S3 put/get/list):

```bash
make e2e-kind
```

Optional environment overrides:
- `KIND_CLUSTER_NAME` (default `pxobj-e2e`)
- `KIND_RECREATE_CLUSTER` (default `true`)
- `PXOBJ_IMAGE` (default `pxobj:e2e`)
- `AWSCLI_IMAGE` (default `amazon/aws-cli:2.17.56`)

## Certificate modes

Default mode: operator-managed certificates.
- Leave `spec.tlsSecretName` unset (or set it to a secret name the operator controls).
- Operator creates a TLS secret containing `tls.crt`, `tls.key`, and `ca.crt` and rotates before expiry.

cert-manager mode:
- Set `spec.useCertManager: true`
- Set `spec.issuerRefName` (and optionally `issuerRefKind`/`issuerRefGroup`)
- Operator reconciles a `cert-manager.io/v1 Certificate` targeting `spec.tlsSecretName`.

In both modes, COSI credential secrets include `AWS_CA_BUNDLE_PEM` for S3 clients.

## Create a bucket and access credentials

```bash
kubectl apply -f deploy/cosi-claim-example.yaml
```

Then read credentials:

```bash
kubectl -n default get secret app-bucket-credentials -o yaml
```

## Important paths

- Operator: `cmd/operator/main.go`
- Object server: `cmd/objectd/main.go`
- COSI controller: `cmd/cosidriver/main.go`
- Reconciler: `controllers/objectservice_controller.go`
- S3 implementation: `internal/s3`
- Metadata/object store: `internal/objectd/store.go`
- COSI listeners: `internal/cosi/listeners.go`

## Notes

- Current S3 protocol support targets core bucket/object workflows used by workloads and COSI lifecycles.
- Credentials are protocol `S3`, auth `Key`, and are bucket-scoped.
- Multi-replica clustering is supported; set `spec.replicas` > 1.

# entity User Guide

This guide covers end-to-end usage of `entity`: installation, configuration, TLS/cert handling, COSI provisioning, S3 client access, scaling, operations, and troubleshooting.

## 1. What You Get

`entity` provides:
- A Kubernetes operator (`ObjectService` CRD)
- A custom S3-compatible object service (no MinIO)
- COSI integration (`Bucket`, `BucketClaim`, `BucketAccess`, classes)
- Clustered operation (`replicas > 1`) with leader-routed writes and quorum replication
- TLS for S3/Admin and mTLS enforcement for internal replication endpoints

## 2. Prerequisites

- Kubernetes cluster (tested with Kind in this repo)
- `kubectl`, `docker`, `go`
- A StorageClass for object data PVCs
- Optional: cert-manager if using `useCertManager: true`

For Portworx, use your Portworx StorageClass (for example `px-repl3`) in `ObjectService.spec.storageClassName`.

## 3. Order Of Operations (Recommended)

Use this exact flow:

1. Build and publish/load image.
2. Apply CRDs (entity + COSI).
3. Apply RBAC/service accounts.
4. Deploy operator.
5. Create `ObjectService`.
6. Wait for StatefulSet and COSI deployment readiness.
7. Create `BucketClass` and `BucketAccessClass`.
8. Create `BucketClaim` and `BucketAccess`.
9. Read generated credentials Secret.
10. Use S3 API (PUT/GET/LIST).

This order avoids reconciliation races and missing-dependency errors.

## 4. Quick Start (Kind)

From repo root:

```bash
make e2e-kind
```

This performs a full integration run:
- creates Kind cluster
- deploys entity
- provisions COSI bucket/access
- runs mTLS-negative check (replication call without client cert must fail `403`)
- runs S3 positive path (`put/get/list`)

## 5. Manual Install

### 5.1 Build

```bash
go build ./...
make docker-build IMAGE=ghcr.io/<your-org>/entity:<tag>
```

## 5A. Install With Helm

### 5A.1 Use Prebuilt Chart From GHCR (Recommended)

`entity` publishes Helm chart artifacts to GHCR OCI:
- `oci://ghcr.io/mchenetz/charts/entity`

Authenticate to GHCR:

```bash
echo <GITHUB_TOKEN> | helm registry login ghcr.io -u <GITHUB_USERNAME> --password-stdin
```

Install operator only:

```bash
helm upgrade --install entity oci://ghcr.io/mchenetz/charts/entity \
  --version 0.1.0 \
  --namespace entity-system --create-namespace
```

Install operator + ObjectService + COSI classes:

```bash
helm upgrade --install entity oci://ghcr.io/mchenetz/charts/entity \
  --version 0.1.0 \
  --namespace entity-system --create-namespace \
  --set image.repository=ghcr.io/mchenetz/entity \
  --set image.tag=latest \
  --set objectService.create=true \
  --set objectService.storageClassName=px-repl3 \
  --set cosi.createClasses=true
```

Optional: pull chart locally first:

```bash
helm pull oci://ghcr.io/mchenetz/charts/entity --version 0.1.0
tar -xzf entity-0.1.0.tgz
helm upgrade --install entity ./entity --namespace entity-system --create-namespace
```

### 5A.2 Use Local Chart From Repo

Operator only (recommended first):

```bash
helm upgrade --install entity ./charts/entity \
  --namespace entity-system --create-namespace
```

Operator + ObjectService + COSI classes:

```bash
helm upgrade --install entity ./charts/entity \
  --namespace entity-system --create-namespace \
  --set image.repository=ghcr.io/<your-org>/entity \
  --set image.tag=<tag> \
  --set objectService.create=true \
  --set objectService.storageClassName=px-repl3 \
  --set cosi.createClasses=true
```

Enable cert-manager mode via Helm:

```bash
helm upgrade --install entity ./charts/entity \
  --namespace entity-system --create-namespace \
  --set objectService.create=true \
  --set objectService.storageClassName=px-repl3 \
  --set objectService.useCertManager=true \
  --set objectService.issuerRefName=entity-ca-issuer
```

### 5.2 Deploy Control Plane

```bash
kubectl apply -f config/crd/bases/entity.io_objectservices.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketclaims.yaml
kubectl apply -f deploy/objectstorage.k8s.io_buckets.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketclasses.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketaccesses.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketaccessclasses.yaml
kubectl apply -f config/rbac/operator-rbac.yaml
kubectl apply -f deploy/operator.yaml
```

If needed, update image in `deploy/operator.yaml` and `ENTITY_IMAGE` env.

### 5.3 Create ObjectService

Example:

```yaml
apiVersion: entity.io/v1alpha1
kind: ObjectService
metadata:
  name: entity
  namespace: entity-system
spec:
  replicas: 3
  storageClassName: px-repl3
  volumeSize: 100Gi
  serviceType: ClusterIP
  port: 9000
  dataPath: /data
```

Apply:

```bash
kubectl apply -f config/samples/entity_v1alpha1_objectservice.yaml
```

Wait:

```bash
kubectl -n entity-system rollout status statefulset/entity
kubectl -n entity-system rollout status deploy/entity-cosi
```

### 5.4 Create COSI Classes

```bash
kubectl apply -f config/samples/cosi-classes.yaml
```

### 5.5 Request Bucket + Access

```bash
kubectl apply -f deploy/cosi-claim-example.yaml
kubectl wait --for=jsonpath='{.status.bucketReady}'=true bucketclaim/app-bucket -n default --timeout=300s
kubectl wait --for=jsonpath='{.status.accessGranted}'=true bucketaccess/app-bucket-access -n default --timeout=300s
```

## 6. TLS And Certificate Handling

### 6.1 Default (Operator-managed self-signed)

If `tlsSecretName` is omitted, operator uses `<name>-tls` and manages cert material:
- `tls.crt`
- `tls.key`
- `ca.crt`

Rotation is automatic before expiry.

### 6.2 cert-manager Mode

Use these `ObjectService.spec` fields:
- `useCertManager: true`
- `issuerRefName: <issuer-or-clusterissuer-name>`
- optional `issuerRefKind` (default `Issuer`)
- optional `issuerRefGroup` (default `cert-manager.io`)
- optional `tlsSecretName`

Example:

```yaml
apiVersion: entity.io/v1alpha1
kind: ObjectService
metadata:
  name: entity
  namespace: entity-system
spec:
  replicas: 3
  storageClassName: px-repl3
  volumeSize: 100Gi
  useCertManager: true
  issuerRefName: entity-ca-issuer
  issuerRefKind: Issuer
  issuerRefGroup: cert-manager.io
```

### 6.3 Internal mTLS

Replication endpoints (`/_cluster/replicate/...`) require:
- valid bearer token
- internal replication header
- verified client certificate

Without client cert, request is rejected with `403` and `mTLS required`.

## 7. Getting Credentials

```bash
kubectl -n default get secret app-bucket-credentials -o yaml
```

Fields include:
- `BUCKET_HOST`
- `BUCKET_NAME`
- `AWS_REGION`
- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- `AWS_CA_BUNDLE_PEM`
- `COSI_BUCKET_INFO`

## 8. S3 Client Examples

### 8.1 AWS CLI

```bash
# decode from secret first
AK=$(kubectl -n default get secret app-bucket-credentials -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)
SK=$(kubectl -n default get secret app-bucket-credentials -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)
HOST=$(kubectl -n default get secret app-bucket-credentials -o jsonpath='{.data.BUCKET_HOST}' | base64 -d)
BUCKET=$(kubectl -n default get secret app-bucket-credentials -o jsonpath='{.data.BUCKET_NAME}' | base64 -d)
REGION=$(kubectl -n default get secret app-bucket-credentials -o jsonpath='{.data.AWS_REGION}' | base64 -d)
CA=$(mktemp)
kubectl -n default get secret app-bucket-credentials -o jsonpath='{.data.AWS_CA_BUNDLE_PEM}' | base64 -d > "$CA"

export AWS_ACCESS_KEY_ID="$AK"
export AWS_SECRET_ACCESS_KEY="$SK"
export AWS_REGION="$REGION"
export AWS_CA_BUNDLE="$CA"

echo hello > hello.txt
aws --endpoint-url "https://$HOST" s3api put-object --bucket "$BUCKET" --key hello.txt --body hello.txt
aws --endpoint-url "https://$HOST" s3api get-object --bucket "$BUCKET" --key hello.txt out.txt
aws --endpoint-url "https://$HOST" s3api list-objects-v2 --bucket "$BUCKET"
```

### 8.2 Python (boto3)

```python
import boto3

s3 = boto3.client(
    "s3",
    endpoint_url="https://<BUCKET_HOST>",
    aws_access_key_id="<AWS_ACCESS_KEY_ID>",
    aws_secret_access_key="<AWS_SECRET_ACCESS_KEY>",
    region_name="<AWS_REGION>",
    verify="/path/to/ca.pem",
)

s3.put_object(Bucket="<BUCKET_NAME>", Key="hello.txt", Body=b"hello")
print(s3.get_object(Bucket="<BUCKET_NAME>", Key="hello.txt")["Body"].read())
```

## 9. Scaling And HA

Set `spec.replicas` to 3+ for quorum replication.

Example patch:

```bash
kubectl -n entity-system patch objectservice entity --type merge -p '{"spec":{"replicas":5}}'
kubectl -n entity-system rollout status statefulset/entity
```

Behavior:
- Reads can be served by any pod.
- Mutating requests are routed to leader.
- Leader replicates to peers and requires quorum acknowledgement.

## 10. Upgrades

Order:
1. Push new image.
2. Update operator deployment image and `ENTITY_IMAGE` env.
3. Let operator roll.
4. Operator reconciles StatefulSet/COSI deployment.

Commands:

```bash
kubectl -n entity-system set image deploy/entity-operator operator=ghcr.io/<org>/entity:<tag>
kubectl -n entity-system set env deploy/entity-operator ENTITY_IMAGE=ghcr.io/<org>/entity:<tag>
kubectl -n entity-system rollout status deploy/entity-operator
kubectl -n entity-system rollout status statefulset/entity
kubectl -n entity-system rollout status deploy/entity-cosi
```

## 11. Security Recommendations

- Keep `serviceType: ClusterIP` unless external access is required.
- Restrict access with NetworkPolicies.
- Rotate `adminToken` periodically.
- Use cert-manager with enterprise PKI when available.
- Scope COSI access classes (`readonly: true`) for read-only consumers.

## 12. Troubleshooting

### 12.1 Operator not reconciling

```bash
kubectl -n entity-system logs deploy/entity-operator
kubectl -n entity-system get objectservice entity -o yaml
```

### 12.2 COSI claim not becoming ready

```bash
kubectl get bucketclaim,bucket,bucketaccess -A -o wide
kubectl -n entity-system logs deploy/entity-cosi
```

### 12.3 TLS failures

Check secret contents:

```bash
kubectl -n entity-system get secret entity-tls -o yaml
```

Verify CA is passed to client (`AWS_CA_BUNDLE_PEM`).

### 12.4 Replication/mTLS issues

```bash
kubectl -n entity-system logs statefulset/entity -c objectd --tail=200
```

Look for replication quorum errors or certificate verification failures.

## 13. Cleanup

```bash
kubectl delete -f deploy/cosi-claim-example.yaml --ignore-not-found
kubectl -n entity-system delete objectservice entity --ignore-not-found
kubectl delete -f config/samples/cosi-classes.yaml --ignore-not-found
kubectl delete -f deploy/operator.yaml --ignore-not-found
kubectl delete -f config/rbac/operator-rbac.yaml --ignore-not-found
kubectl delete -f deploy/objectstorage.k8s.io_bucketaccessclasses.yaml --ignore-not-found
kubectl delete -f deploy/objectstorage.k8s.io_bucketaccesses.yaml --ignore-not-found
kubectl delete -f deploy/objectstorage.k8s.io_bucketclasses.yaml --ignore-not-found
kubectl delete -f deploy/objectstorage.k8s.io_buckets.yaml --ignore-not-found
kubectl delete -f deploy/objectstorage.k8s.io_bucketclaims.yaml --ignore-not-found
kubectl delete -f config/crd/bases/entity.io_objectservices.yaml --ignore-not-found
```

---

Reference files:
- `controllers/objectservice_controller.go`
- `cmd/objectd/main.go`
- `internal/cluster/cluster.go`
- `internal/cluster/replication_handler.go`
- `internal/cosi/listeners.go`
- `hack/e2e-kind.sh`

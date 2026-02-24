#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-entity-e2e}"
ENTITY_IMAGE="${ENTITY_IMAGE:-entity:e2e}"
AWSCLI_IMAGE="${AWSCLI_IMAGE:-amazon/aws-cli:2.17.56}"
KIND_RECREATE_CLUSTER="${KIND_RECREATE_CLUSTER:-true}"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

for c in kind kubectl docker awk sed mktemp; do
  require_cmd "$c"
done

kind_cluster_exists() {
  kind get clusters | grep -qx "$KIND_CLUSTER_NAME"
}

if ! kind_cluster_exists; then
  echo "Creating kind cluster: $KIND_CLUSTER_NAME"
  kind create cluster --name "$KIND_CLUSTER_NAME"
elif [[ "$KIND_RECREATE_CLUSTER" == "true" ]]; then
  echo "Recreating kind cluster: $KIND_CLUSTER_NAME"
  kind delete cluster --name "$KIND_CLUSTER_NAME"
  kind create cluster --name "$KIND_CLUSTER_NAME"
fi

cd "$ROOT_DIR"

echo "Building image: $ENTITY_IMAGE"
docker build -t "$ENTITY_IMAGE" .

echo "Loading image into kind"
kind load docker-image "$ENTITY_IMAGE" --name "$KIND_CLUSTER_NAME"

echo "Applying CRDs and RBAC"
kubectl create namespace entity-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f config/crd/bases/entity.io_objectservices.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketclaims.yaml
kubectl apply -f deploy/objectstorage.k8s.io_buckets.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketclasses.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketaccesses.yaml
kubectl apply -f deploy/objectstorage.k8s.io_bucketaccessclasses.yaml
kubectl apply -f config/rbac/operator-rbac.yaml

operator_manifest="$(mktemp)"
objectservice_manifest=""
client_manifest=""
mtls_manifest=""
trap 'rm -f "$operator_manifest" ${objectservice_manifest:+"$objectservice_manifest"} ${client_manifest:+"$client_manifest"} ${mtls_manifest:+"$mtls_manifest"}' EXIT
sed "s|ghcr.io/mchenetz/entity:latest|$ENTITY_IMAGE|g" deploy/operator.yaml > "$operator_manifest"
kubectl apply -f "$operator_manifest"

kubectl -n entity-system rollout status deploy/entity-operator --timeout=180s

storage_class="$(kubectl get storageclass -o jsonpath='{.items[0].metadata.name}')"
if [[ -z "$storage_class" ]]; then
  echo "No storage class found in cluster" >&2
  exit 1
fi

echo "Using StorageClass: $storage_class"
objectservice_manifest="$(mktemp)"
cat > "$objectservice_manifest" <<MANIFEST
apiVersion: entity.io/v1alpha1
kind: ObjectService
metadata:
  name: entity
  namespace: entity-system
spec:
  replicas: 3
  storageClassName: ${storage_class}
  volumeSize: 5Gi
  serviceType: ClusterIP
  port: 9000
  dataPath: /data
MANIFEST
kubectl apply -f "$objectservice_manifest"

for i in $(seq 1 60); do
  if kubectl -n entity-system get statefulset/entity >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl -n entity-system get statefulset/entity >/dev/null

kubectl -n entity-system rollout status statefulset/entity --timeout=300s
kubectl -n entity-system rollout status deploy/entity-cosi --timeout=300s

kubectl apply -f config/samples/cosi-classes.yaml
kubectl apply -f deploy/cosi-claim-example.yaml

kubectl wait --for=jsonpath='{.status.bucketReady}'=true bucketclaim/app-bucket -n default --timeout=300s
kubectl wait --for=jsonpath='{.status.accessGranted}'=true bucketaccess/app-bucket-access -n default --timeout=300s
kubectl wait --for=condition=Ready pod -n entity-system -l app=entity --timeout=300s
kubectl -n default get secret app-bucket-credentials >/dev/null

admin_token="$(kubectl -n entity-system get secret entity-admin -o jsonpath='{.data.adminToken}' | base64 -d)"
kubectl -n default delete pod entity-mtls-negative --ignore-not-found
mtls_manifest="$(mktemp)"
cat > "$mtls_manifest" <<MANIFEST
apiVersion: v1
kind: Pod
metadata:
  name: entity-mtls-negative
  namespace: default
spec:
  restartPolicy: Never
  containers:
  - name: curl
    image: curlimages/curl:8.12.1
    imagePullPolicy: IfNotPresent
    env:
    - name: ADMIN_TOKEN
      value: "${admin_token}"
    command: ["/bin/sh", "-ec"]
    args:
    - |
      set -euo pipefail
      code=\$(curl -sk -o /tmp/resp -w "%{http_code}" \
        -H "Authorization: Bearer \${ADMIN_TOKEN}" \
        -H "X-ENTITY-Internal-Replication: true" \
        -X POST "https://entity.entity-system.svc.cluster.local:19000/_cluster/replicate/buckets/should-not-create")
      test "\$code" = "403"
      grep -qi "mTLS required" /tmp/resp
MANIFEST
kubectl apply -f "$mtls_manifest"
if ! kubectl -n default wait --for=jsonpath='{.status.phase}'=Succeeded pod/entity-mtls-negative --timeout=180s; then
  kubectl -n default get pod entity-mtls-negative -o wide || true
  kubectl -n default logs pod/entity-mtls-negative || true
  exit 1
fi
kubectl -n default logs pod/entity-mtls-negative >/dev/null

kubectl -n default delete pod entity-e2e-client --ignore-not-found
client_manifest="$(mktemp)"
cat > "$client_manifest" <<MANIFEST
apiVersion: v1
kind: Pod
metadata:
  name: entity-e2e-client
  namespace: default
spec:
  restartPolicy: Never
  containers:
  - name: aws
    image: ${AWSCLI_IMAGE}
    imagePullPolicy: IfNotPresent
    envFrom:
    - secretRef:
        name: app-bucket-credentials
    command: ["/bin/sh", "-ec"]
    args:
    - |
      set -euo pipefail
      echo "entity-e2e-$(date +%s)" > /tmp/hello.txt
      printf '%s\n' "\${AWS_CA_BUNDLE_PEM}" > /tmp/ca.pem
      export AWS_CA_BUNDLE=/tmp/ca.pem
      aws --endpoint-url "https://\${BUCKET_HOST}" --region "\${AWS_REGION}" s3api put-object --bucket "\${BUCKET_NAME}" --key "hello.txt" --body /tmp/hello.txt
      aws --endpoint-url "https://\${BUCKET_HOST}" --region "\${AWS_REGION}" s3api get-object --bucket "\${BUCKET_NAME}" --key "hello.txt" /tmp/out.txt
      diff -u /tmp/hello.txt /tmp/out.txt
      aws --endpoint-url "https://\${BUCKET_HOST}" --region "\${AWS_REGION}" s3api list-objects-v2 --bucket "\${BUCKET_NAME}" --query 'length(Contents)' --output text | awk '{if (\$1 < 1) exit 1}'
MANIFEST
kubectl apply -f "$client_manifest"
if ! kubectl -n default wait --for=jsonpath='{.status.phase}'=Succeeded pod/entity-e2e-client --timeout=300s; then
  kubectl -n default get pod entity-e2e-client -o wide || true
  kubectl -n default logs pod/entity-e2e-client || true
  exit 1
fi

kubectl -n default logs pod/entity-e2e-client

echo "E2E PASSED"

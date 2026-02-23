IMAGE ?= ghcr.io/mchenetz/pxobj:latest

.PHONY: build test docker-build deploy e2e-kind

build:
	go build ./...

test:
	go test ./...

docker-build:
	docker build -t $(IMAGE) .

deploy:
	kubectl apply -f config/crd/bases/pxobj.io_objectservices.yaml
	kubectl apply -f deploy/objectstorage.k8s.io_bucketclaims.yaml
	kubectl apply -f deploy/objectstorage.k8s.io_buckets.yaml
	kubectl apply -f deploy/objectstorage.k8s.io_bucketclasses.yaml
	kubectl apply -f deploy/objectstorage.k8s.io_bucketaccesses.yaml
	kubectl apply -f deploy/objectstorage.k8s.io_bucketaccessclasses.yaml
	kubectl apply -f config/rbac/operator-rbac.yaml
	kubectl apply -f deploy/operator.yaml
	kubectl apply -f config/samples/pxobj_v1alpha1_objectservice.yaml
	kubectl apply -f config/samples/cosi-classes.yaml

e2e-kind:
	./hack/e2e-kind.sh

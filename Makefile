IMAGE ?= cert-manager-webhook-ngcloud
TAG   ?= latest

.PHONY: build test docker-build helm-install

build:
	go build ./...

test:
	TEST_ZONE_NAME=$(TEST_ZONE_NAME) go test ./...

docker-build:
	docker build -t $(IMAGE):$(TAG) .

helm-install:
	helm install cert-manager-webhook-ngcloud deploy/cert-manager-webhook-ngcloud \
	  --namespace cert-manager

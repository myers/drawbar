MODULE := github.com/myers/drawbar
VERSION ?= dev
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(MODULE)/pkg/version.Version=$(VERSION) -X $(MODULE)/pkg/version.GitCommit=$(GIT_COMMIT)
IMAGE ?= localhost:5001/drawbar

.PHONY: build build-controller build-entrypoint test lint image push clean

build: build-controller build-entrypoint

build-controller:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/controller ./cmd/controller/

build-entrypoint:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/entrypoint ./cmd/entrypoint/

test:
	go test ./...

lint:
	golangci-lint run

image:
	docker build -t $(IMAGE):$(VERSION) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) .

push: image
	docker push $(IMAGE):$(VERSION)

push-k3d: image
	docker push $(IMAGE):$(VERSION)

clean:
	rm -rf bin/

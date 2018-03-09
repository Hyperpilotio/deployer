ORGANIZATION=hyperpilot
IMAGE=deployer
TAG=latest
INCLUSTER_TAG=in-cluster
GLIDE=$(which glide)
GO_EXECUTABLE ?= go
# For windows developer, use $(go list ./... | grep -v /vendor/)
PACKAGES=$(glide novendor)

init:
	glide install

test:
	${GO_EXECUTABLE} test ${PACKAGES} -v

dev-test: build
	./deployer --config ./documents/dev.config -logtostderr=true -v=2

build:
	CGO_ENABLED=0 go build -a -installsuffix cgo

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -a -installsuffix cgo

complete-build-linux: init build-linux

run-on-aws:
	./deployer -v 1 --config documents/template.config

run-on-gcp: gcp-create-service-account-file
	./deployer -v 1 --config documents/template.config

docker-build:
	sed -i.bak 's/"inCluster.*/"inCluster": false,/g' documents/deployed.config
	docker build . -t ${ORGANIZATION}/${IMAGE}:${TAG}

docker-build-incluster:
	sed -i.bak 's/"inCluster.*/"inCluster": true,/g' documents/deployed.config
	docker build . -t ${ORGANIZATION}/${IMAGE}:${INCLUSTER_TAG}
docker-push:
	docker push ${ORGANIZATION}/${IMAGE}:${TAG}

gcp-create-service-account-file:
	./scripts/build_gcp_serviceAccoutFile.sh

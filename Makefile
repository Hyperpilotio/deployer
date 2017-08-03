GLIDE=$(which glide)
GO_EXECUTABLE ?= go
# For windows developer, use $(go list ./... | grep -v /vendor/)
PACKAGES=$(glide novendor)

glide-check:
	@if [ "X$(GLIDE)" = "X"	]; then \
		echo "glide doesn't exist."; \
		curl https://glide.sh/get | sh ; \
	else \
		echo "glide installed"; \
	fi

init: glide-check
	glide install

test:
	${GO_EXECUTABLE} test ${PACKAGES}

dev-test: build
	./deployer --config ./documents/dev.config -logtostderr=true -v=2

build:
	CGO_ENABLED=0 go build -a -installsuffix cgo

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -a -installsuffix cgo

docker-build:
	docker build . -t ${ORGANIZATION}/${IMAGE}:${TAG}

docker-push:
	docker push ${ORGANIZATION}/${IMAGE}:${TAG}

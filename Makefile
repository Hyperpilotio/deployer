GLIDE=$(which glide)
GO_EXECUTABLE ?= go
# For windows developer, use $(go list ./... | grep -v /vendor/)
PACKAGES=$(glide novendor)

glide-check:
	@if [ "X$(GLIDE)" = "X"]; then \
		echo "glide doesn't exist."; \
		curl https://glide.sh/get | sh ; \
	else \
		echo "glide installed"; \
	fi

init: glide-check
	glide install
	rm -rf "vendor/k8s.io/client-go/vendor/github.com/golang/glog"

test:
	${GO_EXECUTABLE} test ${PACKAGES}

build:
	${GO_EXECUTABLE} build .

build-docker:
	sudo docker build . -t hyperpilot/deployer

push:
	sudo docker push hyperpilot/deployer:latest	

dev-test: build
	./deployer --config ./documents/dev.config -logtostderr=true -v=2

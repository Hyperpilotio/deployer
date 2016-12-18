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

test:
	${GO_EXECUTABLE} test ${PACKAGES}

build:
	${GO_EXECUTABLE} build .

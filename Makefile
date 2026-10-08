VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARCH    ?= amd64

# Deploy target: make deploy HOST=user@vps DOMAIN=git.example.com [ARCH=arm64]
HOST    ?=
DOMAIN  ?=

.PHONY: build test dist bundle deploy update clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o gitserver ./cmd/gitserver

test:
	go vet ./...
	go test ./...

dist:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/gitserver-linux-amd64 ./cmd/gitserver
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/gitserver-linux-arm64 ./cmd/gitserver

# A self-contained directory to copy to the server.
bundle: dist
	rm -rf dist/bundle && mkdir -p dist/bundle
	cp dist/gitserver-linux-$(ARCH) dist/bundle/gitserver
	cp -r deploy README.md dist/bundle/
	tar -C dist -czf dist/gitserver-$(VERSION)-linux-$(ARCH).tar.gz bundle

# make deploy HOST=user@vps DOMAIN=git.example.com   first install
# make update HOST=user@vps                          update (settings are read from the server)
update: deploy

deploy: bundle
	@test -n "$(HOST)" || (echo "usage: make deploy HOST=user@vps DOMAIN=git.example.com  (DOMAIN is optional for updates)" && exit 1)
	@# Fail early instead of hanging on SSH's first-connection question.
	@host=$$(ssh -G $(HOST) 2>/dev/null </dev/null | awk '/^hostname /{print $$2; exit}'); \
	port=$$(ssh -G $(HOST) 2>/dev/null </dev/null | awk '/^port /{print $$2; exit}'); \
	name=$$host; [ "$$port" = 22 ] || name="[$$host]:$$port"; \
	if ! ssh-keygen -F "$$name" >/dev/null 2>&1; then \
		echo; \
		echo "This computer has not connected to $$name before, so SSH would stop and"; \
		echo "ask you to confirm the server's host key. Do that once, then re-run:"; \
		echo; \
		echo "    ssh $(HOST)        # check the fingerprint, answer 'yes', then exit"; \
		echo; \
		exit 1; \
	fi
	scp -q -o ConnectTimeout=15 dist/gitserver-$(VERSION)-linux-$(ARCH).tar.gz $(HOST):/tmp/gitserver-bundle.tar.gz
	ssh -t -o ConnectTimeout=15 $(HOST) 'set -e; rm -rf /tmp/gitserver-bundle; mkdir /tmp/gitserver-bundle; \
		tar -C /tmp/gitserver-bundle --strip-components=1 -xzf /tmp/gitserver-bundle.tar.gz; \
		sudo $(if $(DOMAIN),DOMAIN=$(DOMAIN)) $(if $(RECONFIGURE),RECONFIGURE=$(RECONFIGURE)) sh /tmp/gitserver-bundle/deploy/install.sh; \
		rm -rf /tmp/gitserver-bundle /tmp/gitserver-bundle.tar.gz'

clean:
	rm -rf dist gitserver

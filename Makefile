.PHONY: build clean deploy test lint

BINARY_DIR := bin

build: $(BINARY_DIR)/webhook

$(BINARY_DIR)/webhook: cmd/webhook/main.go internal/**/*.go
	go build -o $@ ./cmd/webhook

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -rf $(BINARY_DIR)

deploy: build
	scp $(BINARY_DIR)/webhook runner-host:/tmp/github-runners-webhook
	scp cloud-init/runner.yaml.tmpl runner-host:/tmp/github-runners-cloud-init
	scp deploy/webhook.service runner-host:/tmp/github-runners-webhook.service
	ssh runner-host 'sudo systemctl disable --now cleanup.timer >/dev/null 2>&1 || true; sudo install -d -m 0755 /opt/github-runners/cloud-init && sudo install -m 0755 /tmp/github-runners-webhook /usr/local/bin/webhook && sudo install -m 0644 /tmp/github-runners-cloud-init /opt/github-runners/cloud-init/runner.yaml.tmpl && sudo install -m 0644 /tmp/github-runners-webhook.service /etc/systemd/system/webhook.service && sudo systemctl daemon-reload && sudo systemctl enable --now webhook'

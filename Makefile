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
	ssh runner-host 'sudo systemctl disable --now cleanup.timer >/dev/null 2>&1 || true; sudo install -d -m 0755 /opt/github-runners/cloud-init'
	ssh runner-host 'sudo install -m 0755 /dev/stdin /usr/local/bin/webhook' < $(BINARY_DIR)/webhook
	ssh runner-host 'sudo install -m 0644 /dev/stdin /opt/github-runners/cloud-init/runner.yaml.tmpl' < cloud-init/runner.yaml.tmpl
	ssh runner-host 'sudo install -m 0644 /dev/stdin /etc/systemd/system/webhook.service' < deploy/webhook.service
	ssh runner-host 'sudo systemctl daemon-reload && sudo systemctl enable webhook && sudo systemctl restart webhook'

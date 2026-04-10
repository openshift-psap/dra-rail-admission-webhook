IMG ?= dra-gpu-nic-webhook:latest
RECONCILER_IMG ?= dra-gpu-nic-reconciler:latest
NAMESPACE ?= dra-webhook-system
E2E_KUBECONFIG ?= $(HOME)/.kube/config

.PHONY: build test e2e docker-build docker-push deploy undeploy generate-certs

build:
	go build -o bin/webhook ./cmd/webhook/
	go build -o bin/reconciler ./cmd/reconciler/

test:
	go test ./... -v

docker-build:
	docker build -t $(IMG) --target webhook .
	docker build -t $(RECONCILER_IMG) --target reconciler .

docker-build-unified:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)
	docker push $(RECONCILER_IMG)

deploy: generate-certs
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f deploy/ -n $(NAMESPACE)

undeploy:
	kubectl delete -f deploy/ -n $(NAMESPACE) --ignore-not-found

generate-certs:
	@echo "Generating self-signed TLS certificates..."
	@mkdir -p certs
	@openssl req -x509 -newkey rsa:4096 -keyout certs/tls.key -out certs/tls.crt \
		-days 365 -nodes -subj "/CN=dra-gpu-nic-webhook.$(NAMESPACE).svc" \
		-addext "subjectAltName=DNS:dra-gpu-nic-webhook.$(NAMESPACE).svc,DNS:dra-gpu-nic-webhook.$(NAMESPACE).svc.cluster.local"
	@kubectl create secret tls dra-gpu-nic-webhook-tls \
		--cert=certs/tls.crt --key=certs/tls.key \
		-n $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	@echo "Certificates generated. Update deploy/webhook-config.yaml caBundle with:"
	@echo "$$(cat certs/tls.crt | base64 | tr -d '\n')"

e2e:
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) \
	go test -v -tags e2e -timeout 45m -count 1 ./test/e2e/

clean:
	rm -f bin/webhook bin/reconciler
	rm -rf certs/

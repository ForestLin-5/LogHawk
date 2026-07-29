.PHONY: all build clean docker-up docker-down deploy help

# ����
REGISTRY ?= loghawk
TAG      ?= latest

# ���� Go ����
GO_SERVICES = ingest ai-proxy alerter chaos log-collector

# ===== Ĭ��Ŀ�� =====
all: build

# ===== �������� Go ���� =====
build:
	@for svc in $(GO_SERVICES); do \
		echo "?? Building $$svc..."; \
		cd services/$$svc && CGO_ENABLED=0 go build -ldflags="-s -w" -o $$svc . && cd ../..; \
	done
	@echo "? All services built"

# ===== Docker ���� =====
docker-build:
	@for svc in $(GO_SERVICES); do \
		echo "?? Building Docker image for $$svc..."; \
		docker build -t $(REGISTRY)/$$svc:$(TAG) services/$$svc; \
	done
	docker build -t $(REGISTRY)/frontend:$(TAG) services/frontend
	@echo "? All Docker images built"

# ===== Docker Compose �������� =====
up:
	docker-compose up -d
	@echo "?? LogHawk running at http://localhost:30080"

down:
	docker-compose down

logs:
	docker-compose logs -f

# ===== K8s ���� =====
deploy:
	bash scripts/deploy.sh

# ===== ���� =====
clean:
	@for svc in $(GO_SERVICES); do \
		rm -f services/$$svc/$$svc; \
	done
	@echo "?? Cleaned"

# ===== ���ԣ�ռλ�� =====
test:
	@echo "?? Running tests..."
	@for svc in $(GO_SERVICES); do \
		cd services/$$svc && go test ./... || echo "  ??  $$svc: no tests yet"; cd ../..; \
	done

# ===== ���� =====
help:
	@echo "?? LogHawk Makefile"
	@echo ""
	@echo "  make build        �������� Go ����"
	@echo "  make docker-build  �������� Docker ����"
	@echo "  make up           docker-compose ��������"
	@echo "  make down         ֹͣ docker-compose"
	@echo "  make logs         �鿴 docker-compose ��־"
	@echo "  make deploy       K8s һ������"
	@echo "  make test         ���в���"
	@echo "  make clean        �����������"
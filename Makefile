BINARY := channel-service
ENV_FILE := .env

.PHONY: start stop restart build

## 启动服务（若已运行则先 kill 再启动）
start:
	@if pgrep -x "$(BINARY)" > /dev/null 2>&1; then \
		echo "Stopping existing $(BINARY) process..."; \
		pkill -x "$(BINARY)"; \
		sleep 1; \
	fi
	@if [ -f "$(ENV_FILE)" ]; then \
		export $$(grep -v '^#' $(ENV_FILE) | xargs); \
	fi
	@echo "Starting $(BINARY)..."
	@export $$(grep -v '^#' $(ENV_FILE) | xargs) && nohup ./$(BINARY) > nohup.out 2>&1 &
	@echo "$(BINARY) started. Logs: nohup.out"

## 停止服务
stop:
	@if pgrep -x "$(BINARY)" > /dev/null 2>&1; then \
		pkill -x "$(BINARY)"; \
		echo "$(BINARY) stopped."; \
	else \
		echo "$(BINARY) is not running."; \
	fi

## 重启服务
restart: stop start

## 构建二进制
build:
	go build -o $(BINARY) ./pkg/cmd/main.go

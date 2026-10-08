VERSION ?= 1.0.0
# Адрес сервера, который зашивается в клиент и попадает в QR. По умолчанию —
# IP машины в локальной сети, чтобы QR открывался с телефона.
LAN_IP  := $(or $(shell ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<NF;i++) if($$i=="src") print $$(i+1)}'),$(shell ipconfig getifaddr en0 2>/dev/null))
PORT    ?= 8780
SERVER  ?= http://$(or $(LAN_IP),localhost):$(PORT)
RUN_DIR ?= demo
UPX     ?= 0

# Интервалы для стенда (в проде по умолчанию 600 с). Данные стенда — в $(RUN_DIR)/data.
DEMO_ENV = SHOWCASE_SERVER=$(SERVER) SHOWCASE_PRODUCT_INTERVAL=15 SHOWCASE_UPDATE_INTERVAL=10 \
	SHOWCASE_DATA_DIR=$(RUN_DIR)/data

HOST_OS := $(shell uname -s)
ifeq ($(HOST_OS),Darwin)
RUN_BIN := dist/showcase-$(VERSION)-macos-$(shell uname -m | sed s/arm64/aarch64/)
else
RUN_BIN := dist/showcase-$(VERSION)-linux-x86_64
endif

.PHONY: server-up server-down server-logs test client client-macos release run sizes clean

server-up:            ## поднять сервер стенда (http://localhost:$(PORT)/admin)
	PORT=$(PORT) docker compose up -d --build

server-down:
	docker compose down

server-logs:
	docker compose logs -f server

test:                 ## тесты сервера (Go) и клиента (Rust) в контейнерах
	docker run --rm -v $(CURDIR)/server:/src -w /src golang:1.27.1-alpine go test -count=1 ./...
	docker run --rm -v showcase-cargo:/usr/local/cargo/registry -v showcase-target:/src/target \
		-v $(CURDIR)/client:/src -w /src rust:1.99.0-slim-bookworm cargo test --locked

client:               ## собрать клиент VERSION под linux и windows в dist/ (Docker)
	DOCKER_BUILDKIT=1 docker build client --progress=plain \
		--build-arg VERSION=$(VERSION) --build-arg DEFAULT_SERVER=$(SERVER) --build-arg COMPRESS=$(UPX) \
		--output type=local,dest=dist
	@ls -l dist/showcase-$(VERSION)-*

client-macos:         ## собрать клиент VERSION под macOS arm64 + x86_64 (только на Mac)
	VERSION=$(VERSION) SERVER=$(SERVER) ./scripts/build-macos.sh

release: client       ## собрать и опубликовать VERSION на сервере обновлений
	$(if $(filter Darwin,$(HOST_OS)),$(MAKE) client-macos VERSION=$(VERSION))
	cp dist/showcase-$(VERSION)-* releases/
	@echo "Опубликована $(VERSION): клиенты обновятся при следующей проверке"

run:                  ## запустить клиент VERSION (во весь экран) из каталога $(RUN_DIR)
	mkdir -p $(RUN_DIR)
	cp -n $(RUN_BIN) $(RUN_DIR)/showcase || true
	$(DEMO_ENV) ./$(RUN_DIR)/showcase

sizes:
	@ls -l dist | awk 'NR>1 {printf "%-40s %6.2f MB\n", $$9, $$5/1048576}'

clean:
	rm -rf dist/* releases/* $(RUN_DIR)

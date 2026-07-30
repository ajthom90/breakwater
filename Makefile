.PHONY: test test-short test-gate test-m1 build cross docker tidy web web-typecheck

test-short:
	cd server && go test ./... -count=1 -short -timeout 10m
	cd pkg && go test ./... -count=1

test-m1:
	cd server && go test ./internal/agentgw/ -count=1 -run TestM1_EnrollmentAndWrongCertRejection -v

test-gate:
	cd server && go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -timeout 45m -v

test: test-short test-m1

# Build the React UI into server/internal/web/dist for go:embed.
# Backend-only contributors: a committed placeholder dist/index.html keeps
# `go build ./...` working without Node. Run `make web` for the real shell.
web:
	cd web && npm ci && npm run build

web-typecheck:
	cd web && npm ci && npx tsc --noEmit -p tsconfig.app.json

build: web
	cd server && CGO_ENABLED=0 go build -o ../bin/breakwaterd ./cmd/breakwaterd

# Server binary only (uses whatever is already in server/internal/web/dist).
build-server:
	cd server && CGO_ENABLED=0 go build -o ../bin/breakwaterd ./cmd/breakwaterd

cross:
	cd server && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../bin/breakwaterd.exe ./cmd/breakwaterd
	cd agent && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ../bin/breakwater-agent.exe ./cmd/breakwater-agent

docker:
	docker build -f packaging/docker/Dockerfile -t breakwater:dev .

tidy:
	cd pkg && go mod tidy
	cd server && go mod tidy
	cd agent && go mod tidy
	cd restore && go mod tidy
	cd cli && go mod tidy
	go work sync

proto:
	protoc -I proto \
	  --go_out=pkg/proto --go_opt=paths=source_relative \
	  --go-grpc_out=pkg/proto --go-grpc_opt=paths=source_relative \
	  proto/breakwater/v1/breakwater.proto

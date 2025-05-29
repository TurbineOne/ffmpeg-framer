
.PHONY: all clean

# Binaries
BINARIES = framer-server

# Go build command
GO_BUILD = go build -o

PROTOC_OPTS = --go_out=gen --go_opt=paths=source_relative --go-grpc_out=gen --go-grpc_opt=paths=source_relative

all: $(BINARIES)

protos: proto/fps/model/*.proto
	mkdir -p gen/ && protoc --proto_path=proto $(PROTOC_OPTS) proto/fps/model/*.proto proto/fps/service/*.proto proto/fps/*.proto
#	go mod tidy

$(BINARIES): protos
	$(GO_BUILD) ./out/$@ ./cmd/$@/main.go

clean:
	rm -rf ./out/ ./gen/ $(BINARIES)


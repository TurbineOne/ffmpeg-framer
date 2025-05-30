
.PHONY: all clean

# Binaries
BINARIES = framer-server

# Go build command
GO_BUILD = go build -o

PROTOC_OPTS = --go_out=api/proto/gen/go --go_opt=paths=source_relative --go-grpc_out=api/proto/gen/go --go-grpc_opt=paths=source_relative

all: $(BINARIES)

protos: api/proto/fps/model/*.proto api/proto/fps/service/*.proto api/proto/fps/*.proto
	mkdir -p api/proto/gen/go && protoc --proto_path=api/proto $(PROTOC_OPTS) api/proto/fps/model/*.proto api/proto/fps/service/*.proto api/proto/fps/*.proto

$(BINARIES): protos
	$(GO_BUILD) ./out/$@ ./cmd/$@/main.go

clean:
	rm -rf ./out/ ./gen/ $(BINARIES)

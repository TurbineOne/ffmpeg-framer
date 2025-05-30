FROM ubuntu:22.04

ENV GOLANG_VERSION=1.23.9
ENV GOLANG_CHECKSUM=de03e45d7a076c06baaa9618d42b3b6a0561125b87f6041c6397680a71e5bb26
ENV GOLANG_ARCH=linux-amd64

# ENV GOLANG_CHECKSUM=3dc4dd64bdb0275e3ec65a55ecfc2597009c7c46a1b256eefab2f2172a53a602
# ENV GOLANG_ARCH=linux-arm64

ENV PROTOC_VERSION=29.3
ENV PROTOC_CHECKSUM=3e866620c5be27664f3d2fa2d656b5f3e09b5152b42f1bedbf427b333e90021a
ENV PROTOC_ARCH=linux-x86_64

# ENV PROTOC_CHECKSUM=6427349140e01f06e049e707a58709a4f221ae73ab9a0425bc4a00c8d0e1ab32
# ENV PROTOC_ARCH=linux-aarch_64

RUN apt-get update && apt-get install -y \
        wget \
        build-essential \
        pkg-config \
        unzip \
        ffmpeg \
        libavcodec-dev \
        libavdevice-dev \
        libavfilter-dev \
        libavformat-dev \
        libavutil-dev \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Install golang
RUN wget -q -O /tmp/go${GOLANG_VERSION}.${GOLANG_ARCH}.tar.gz https://go.dev/dl/go${GOLANG_VERSION}.${GOLANG_ARCH}.tar.gz \
    && echo "${GOLANG_CHECKSUM} /tmp/go${GOLANG_VERSION}.${GOLANG_ARCH}.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go${GOLANG_VERSION}.${GOLANG_ARCH}.tar.gz \
    && rm /tmp/go${GOLANG_VERSION}.${GOLANG_ARCH}.tar.gz

RUN wget -q -O /tmp/protoc-${PROTOC_VERSION}-${PROTOC_ARCH}.zip \
    https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-${PROTOC_ARCH}.zip \
    && echo "${PROTOC_CHECKSUM} /tmp/protoc-${PROTOC_VERSION}-${PROTOC_ARCH}.zip" | sha256sum -c - \
    && unzip -d /usr/local /tmp/protoc-${PROTOC_VERSION}-${PROTOC_ARCH}.zip \
    && rm /tmp/protoc-${PROTOC_VERSION}-${PROTOC_ARCH}.zip

ENV PATH=${PATH}:/usr/local/go/bin
ENV GOOS=linux
ENV GOROOT=/usr/local/go
ENV GOBIN=/usr/local/go/bin

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest \
    && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

WORKDIR /app

# Cache dependencies in a separate layer
ADD go.mod go.mod
ADD go.sum go.sum
RUN go mod download

# Build the application
ADD . ./
RUN make && mkdir /app/bin && cp /app/out/* /app/bin/

ENV PATH=${PATH}:/app/bin
WORKDIR /app

ENTRYPOINT ["/bin/bash", "-c"]

CMD ["/app/bin/framer-server"]

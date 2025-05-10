# --------------------------------------------------------------------------------
# 1) Builder stage: compile a static binary
# --------------------------------------------------------------------------------
    FROM golang:1.24.2-alpine AS builder

    # ensure we can fetch private deps if needed (not required here)
    # RUN apk add --no-cache git
    
    WORKDIR /app
    
    # copy and download deps first for caching
    COPY go.mod go.sum ./
    RUN go mod download
    
    # copy rest of the sources and your secrets
    COPY . .
    
    # compile statically for Linux
    # CGO_ENABLED=0 makes the binary fully static
    RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -o mesh-server server.go
    
    # --------------------------------------------------------------------------------
    # 2) Final image: tiny, no Go, just the certs and your binary
    # --------------------------------------------------------------------------------
    FROM alpine:3.18
    
    # for firestore TLS
    RUN apk add --no-cache ca-certificates
    
    WORKDIR /app
    
    # copy in the compiled binary and the secrets folder
    COPY --from=builder /app/mesh-server .
    COPY --from=builder /app/secrets ./secrets
    
    # expose your service port
    EXPOSE 8443
    
    # optional healthcheck
    HEALTHCHECK --interval=30s --timeout=3s \
      CMD wget -qO- http://localhost:8443/healthz || exit 1
    
    # drop privileges (you can adjust UID/GID as you like)
    RUN addgroup -S app && adduser -S -G app app
    USER app
    
    # run!
    ENTRYPOINT ["./mesh-server"]
    
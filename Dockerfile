FROM golang:1.27-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /build/module ./cmd/module

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/module /
EXPOSE 8475 9475
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/module", "--health-check"]
ENTRYPOINT ["/module"]

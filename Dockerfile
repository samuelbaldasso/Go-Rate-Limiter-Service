FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/api ./cmd/api

FROM scratch
COPY --from=builder /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]

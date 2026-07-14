FROM golang:1.26.4-alpine3.23@sha256:eb5a920799142c2fe9ec705cfa0ebcc4380e2e2f041b84e9ae5c6d82a2e56c82 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o webhook .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
COPY --from=builder /app/webhook /webhook
ENTRYPOINT ["/webhook"]

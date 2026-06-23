FROM golang:1.25.11-alpine3.23@sha256:ba6ba81bf21dfcccdb8c9832c5f09d775cd19a6ed1b323b3220949361958ca30 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o webhook .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=builder /app/webhook /webhook
ENTRYPOINT ["/webhook"]

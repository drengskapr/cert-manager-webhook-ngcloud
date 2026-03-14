FROM golang:1.25.6 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o webhook .

FROM gcr.io/distroless/static
COPY --from=builder /app/webhook /webhook
ENTRYPOINT ["/webhook"]

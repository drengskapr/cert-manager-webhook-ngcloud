FROM golang:1.27rc2-alpine3.23@sha256:ae3270df82e02c950e7cc87b8e3c5d8f25a9c9006de73fa029f27b4cdcecc68f AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o webhook .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=builder /app/webhook /webhook
ENTRYPOINT ["/webhook"]

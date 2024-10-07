FROM golang:1.22.5-alpine3.20 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o udp-fwd .

FROM alpine:3.20.0 AS runtime
WORKDIR /app
COPY --from=builder /app/udp-fwd .
EXPOSE 8125/udp
EXPOSE 8080

CMD ["./udp-fwd"]
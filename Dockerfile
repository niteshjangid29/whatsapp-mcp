FROM golang:1.24-alpine

# Install sqlite3
RUN apk add --no-cache gcc musl-dev sqlite-dev

ENV CGO_ENABLED=1

WORKDIR /app

COPY whatsapp-bridge/go.mod whatsapp-bridge/go.sum ./

RUN go mod download

COPY whatsapp-bridge/ .

RUN go build -o main .

EXPOSE 6000

CMD ["./main"]
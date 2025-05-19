FROM golang:1.24-alpine

# Install sqlite3
RUN apk add --no-cache sqlite sqlite-dev

WORKDIR /app

COPY . .

RUN go mod download

RUN go build -o main .

EXPOSE 8080

CMD ["./main"]

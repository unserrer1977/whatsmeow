FROM golang:1.27-bookworm AS build

WORKDIR /app

COPY . .

RUN go mod tidy

RUN CGO_ENABLED=1 go build -o app .

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /app/app /app/app

ENV DATA_DIR=/data

EXPOSE 8080

CMD ["/app/app"]

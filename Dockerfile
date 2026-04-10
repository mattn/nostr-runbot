FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/nostr-runbot .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app
USER app
COPY --from=build /out/nostr-runbot /usr/local/bin/nostr-runbot

ENV ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nostr-runbot"]

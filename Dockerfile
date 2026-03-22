# syntax = docker/dockerfile:latest
FROM golang:1.23-alpine3.19 AS build
RUN apk add --update build-base gcc wget git libpcap-dev
WORKDIR /app
COPY src/* /app/
COPY game_quotes.json /app/
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  go mod tidy; \
  CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o lazytainer ./...

FROM alpine
RUN apk add --update libpcap-dev
COPY --from=build /app/lazytainer /app/lazytainer
COPY --from=build /app/status_page.css /app/status_page.css
COPY --from=build /app/game_quotes.json /app/game_quotes.json
ENTRYPOINT [ "./app/lazytainer" ]


FROM golang:1.26.11-alpine AS builder
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api

FROM alpine:3.24
RUN addgroup -S -g 1001 kamui && adduser -S -u 1001 -G kamui kamui \
    && mkdir /data && touch /data/.kamui && chown -R kamui:kamui /data
COPY --from=builder /out/api /usr/local/bin/kamui-api
USER 1001:1001
EXPOSE 8080
ENTRYPOINT ["kamui-api"]

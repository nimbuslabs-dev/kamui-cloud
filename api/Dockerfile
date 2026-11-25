# Kamui API - Go Application
# Multi-stage build for production

# Stage 1: Build dependencies
FROM golang:1.22-alpine AS deps
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download

# Stage 2: Build the application
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY --from=deps /app/go.mod /app/go.sum ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o api ./cmd/api

# Stage 3: Production runner
FROM alpine:3.20
WORKDIR /app

# Install wget for healthcheck
RUN apk add --no-cache wget

# Create non-root user
RUN addgroup --system --gid 1001 api
RUN adduser --system --uid 1001 --ingroup api api

# Copy binary
COPY --from=builder /app/api .

# Change ownership
RUN chown api:api /app/api

USER api

EXPOSE 8080

ENV PORT=8080

CMD ["./api"]
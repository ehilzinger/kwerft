# syntax=docker/dockerfile:1

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=0.1.0-dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/ehilzinger/werft/internal/version.Version=${VERSION} -X github.com/ehilzinger/werft/internal/version.Commit=${COMMIT}" \
    -o /out/werft ./cmd/werft

FROM gcr.io/distroless/static:nonroot
COPY --from=go /out/werft /usr/local/bin/werft
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/werft"]

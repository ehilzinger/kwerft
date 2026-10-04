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
    -ldflags "-s -w -X github.com/ehilzinger/kwerft/internal/version.Version=${VERSION} -X github.com/ehilzinger/kwerft/internal/version.Commit=${COMMIT}" \
    -o /out/kwerft ./cmd/kwerft

FROM gcr.io/distroless/static:nonroot
COPY --from=go /out/kwerft /usr/local/bin/kwerft
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kwerft"]

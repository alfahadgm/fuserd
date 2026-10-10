# syntax=docker/dockerfile:1

FROM golang:1.26.9-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fuserd ./cmd/fuserd

# Build with: docker build --output type=local,dest=build .
# This exports build/fuserd to the host without packaging a runtime image.
FROM scratch
COPY --from=build /out/fuserd /fuserd

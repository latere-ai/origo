# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

# One binary, no build step, no bundler, and nothing on disk beside it: the
# stylesheet, the font and the templates are compiled in.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${BUILD_DATE}" \
    -o /out/origoweb ./cmd/origoweb

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/origoweb /usr/local/bin/origoweb
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/origoweb"]

# Build the sal-pub server as a static binary.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
COPY pkg ./pkg
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sal-pub ./cmd/server

# Distroless static includes CA certificates (needed to fetch remote actors over HTTPS).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sal-pub /sal-pub
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/sal-pub"]

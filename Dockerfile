# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/macfleet-server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/macfleet-server /macfleet-server
USER 65532:65532
EXPOSE 8080 8443
ENTRYPOINT ["/macfleet-server"]

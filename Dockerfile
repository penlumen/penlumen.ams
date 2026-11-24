# Builds one static binary (templates, styles and migrations included) and
# ships it on a minimal image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ams ./cmd/ams

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ams /ams
EXPOSE 8080
ENTRYPOINT ["/ams"]

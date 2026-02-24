FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/entity-operator ./cmd/operator && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/entity-objectd ./cmd/objectd && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/entity-cosidriver ./cmd/cosidriver

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/entity-operator /entity-operator
COPY --from=build /out/entity-objectd /entity-objectd
COPY --from=build /out/entity-cosidriver /entity-cosidriver
ENTRYPOINT ["/entity-operator"]

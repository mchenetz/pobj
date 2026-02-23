FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/pxobj-operator ./cmd/operator && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/pxobj-objectd ./cmd/objectd && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/pxobj-cosidriver ./cmd/cosidriver

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pxobj-operator /pxobj-operator
COPY --from=build /out/pxobj-objectd /pxobj-objectd
COPY --from=build /out/pxobj-cosidriver /pxobj-cosidriver
ENTRYPOINT ["/pxobj-operator"]

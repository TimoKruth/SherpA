FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY deploy/web/testfixture/main.go ./main.go
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /fixture ./main.go

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f

COPY --from=build /fixture /fixture
USER nonroot:nonroot
ENTRYPOINT ["/fixture"]

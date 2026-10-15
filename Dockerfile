FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /filerelay .

FROM alpine:3.22
RUN adduser -D -H -u 10001 relay && mkdir /data && chown relay /data
COPY --from=build /filerelay /usr/local/bin/filerelay
USER relay
VOLUME /data
EXPOSE 8080
ENV RELAY_DATA=/data RELAY_LISTEN=:8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["filerelay"]

FROM golang:1.16 AS builder

ENV GO111MODULE=on
ENV GOFLAGS=-mod=vendor

RUN groupadd builder
RUN useradd -m -g builder -l builder
RUN mkdir -p /builder
RUN chown -R builder:builder /builder

WORKDIR /builder
USER builder
COPY . .
RUN go build watchurl.go

FROM debian:buster

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates
RUN update-ca-certificates

RUN groupadd watchurl 
RUN useradd -m -g watchurl -l watchurl
RUN mkdir -p /watchurl

COPY --chown=0:0 --from=builder /builder/watchurl /watchurl/watchurl

WORKDIR /watchurl
USER watchurl
ENTRYPOINT ["./watchurl", "--state-dir=/mnt/data", "--logtostderr"]

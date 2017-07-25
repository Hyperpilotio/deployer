FROM alpine:3.4

RUN mkdir /lib64 && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2
RUN apk --update upgrade && \
    apk add curl ca-certificates && \
    update-ca-certificates && \
    rm -rf /var/cache/apk/*

ENV GOPATH /opt/deployer

COPY deployer /opt/deployer/deployer
COPY ./documents/deployed.config /etc/deployer/config.json
COPY ./ui/ /opt/deployer/src/github.com/hyperpilotio/deployer/ui/

CMD ["/opt/deployer/deployer", "-v", "1", "-logtostderr"]

FROM golang:1.8.1-alpine

RUN mkdir /lib64 && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2
RUN mkdir -p /etc/deployer

COPY ./ui/ /go/src/github.com/hyperpilotio/deployer/ui/
ADD ./documents/config.json /etc/deployer/config.json
ADD deployer /usr/local/bin/deployer

ENTRYPOINT deployer

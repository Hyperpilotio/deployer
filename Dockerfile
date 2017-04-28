FROM alpine:3.4

RUN apk --no-cache add ca-certificates && update-ca-certificates
COPY deployer /usr/local/bin/deployer
COPY ./documents/config.json /etc/deployer/
COPY ./ui /ui

EXPOSE 7777

ENTRYPOINT ["deployer"]

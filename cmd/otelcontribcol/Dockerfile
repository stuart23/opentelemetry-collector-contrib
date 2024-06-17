FROM alpine:latest as prep
RUN apk --update add ca-certificates

RUN mkdir -p /tmp

FROM scratch

ARG USER_UID=10001
USER ${USER_UID}

COPY --from=prep /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY otelcontribcol /
EXPOSE 4317 55680 55679 40000


# Start Delve
CMD ./dlv --listen=:40000 --headless=true --api-version=2 --log exec ./otelcontribcol

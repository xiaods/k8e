FROM golang:1.15.2-alpine3.12

RUN apk --no-cache add curl

COPY bin/k8e /

COPY bin/host-local /usr/local/bin/

WORKDIR /

CMD /k8e

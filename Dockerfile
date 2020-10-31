FROM golang:1.15.2-alpine3.12

COPY bin/k8e /

WORKDIR /

CMD /k8e

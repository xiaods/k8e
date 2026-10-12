FROM python:3.11.2-slim@sha256:2f749ef90f54fd4b3c77cde78eec23ab5b8199d9ac84e4ced6ae523ef223ef7b

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /opt/grpc-http2-test

COPY third_party/grpc-http2-test/requirements.txt /tmp/requirements.txt
COPY third_party/grpc-http2-test/wheels/ /tmp/wheels/
RUN python -m pip install \
      --no-cache-dir \
      --no-index \
      --only-binary=:all: \
      --require-hashes \
      --find-links=/tmp/wheels \
      --requirement=/tmp/requirements.txt \
    && python -m pip check \
    && rm -rf /tmp/requirements.txt /tmp/wheels

COPY third_party/grpc-http2-test/*.py ./
COPY third_party/grpc-http2-test/LICENSE \
     third_party/grpc-http2-test/licenses/ \
     /usr/share/licenses/grpc-http2-test/

USER 65532:65532
ENTRYPOINT ["python", "/opt/grpc-http2-test/http2_test_server.py"]
CMD ["--base_port=8080"]

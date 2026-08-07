# Stdlib-only: http.client and http.server both ship with CPython, so there is
# nothing to install and no lockfile to keep in step.
FROM python:3.12-alpine3.21

# Unprivileged, and it needs nothing more: this process never sees the Docker
# socket. It speaks plain HTTP to docker-socket-proxy, which is the only thing
# in the stack holding the socket and which permits GET on /containers/* alone.
USER 65534:65534

WORKDIR /app
COPY exporter.py .

EXPOSE 9105

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD python3 -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:9105/metrics').read()"

ENTRYPOINT ["python3", "-u", "exporter.py"]

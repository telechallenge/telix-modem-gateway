# Stdlib-only: sqlite3 and http.server both ship with CPython, so there is
# nothing to install and no lockfile to keep in step.
FROM python:3.12-alpine3.21

# Runs as PUID:PGID (1000:1000 by default), the uid bbs_infra's `make init`
# chowns the instance data to -- not root, and not nobody. The databases are
# 0644 today so nobody would also work, but a sysop who tightens them to 0640
# would silently take the exporter down; running as their owner survives that.
# Everything else is taken away in docker-compose.yml: every capability
# dropped, no-new-privileges, a read-only root filesystem, and the only mount
# is the instances directory, read-only. The process opens every database with
# SQLite's mode=ro URI, so it cannot alter a board's state even in principle.
USER 1000:1000

WORKDIR /app
COPY exporter.py .

EXPOSE 9104

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD python3 -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:9104/metrics').read()"

ENTRYPOINT ["python3", "-u", "exporter.py"]

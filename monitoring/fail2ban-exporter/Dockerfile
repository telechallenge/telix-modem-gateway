# Stdlib-only: sqlite3 and http.server both ship with CPython, so there is
# nothing to install and no lockfile to keep in step.
FROM python:3.12-alpine3.21

# Runs as root, which is not an oversight: fail2ban creates its database
# root-owned at mode 0600, so an unprivileged reader gets "unable to open
# database file" and the exporter reports itself down. Everything that can be
# taken away instead is, in docker-compose.yml — every capability dropped,
# no-new-privileges, a read-only root filesystem, and the only mount is
# fail2ban's data volume, read-only. The process opens the database with
# SQLite's mode=ro URI and never touches the fail2ban socket, so it cannot
# alter ban state even in principle.

WORKDIR /app
COPY exporter.py .

EXPOSE 9102

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD python3 -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:9102/metrics').read()"

ENTRYPOINT ["python3", "-u", "exporter.py"]

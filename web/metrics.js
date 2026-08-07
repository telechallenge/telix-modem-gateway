// Prometheus metrics for the web terminal proxy.
//
// Hand-rolled rather than using prom-client, for a build reason rather than a
// stylistic one: the Docker image installs with `npm ci`, which aborts when
// package.json and package-lock.json disagree. Adding a dependency therefore
// requires regenerating the lock file, and the metric set here is a handful of
// counters and gauges with no histograms or quantiles — the part of the
// exposition format that is genuinely fiddly. The Go gateway, whose metrics do
// include histograms and which benefits from the runtime collectors, uses
// prometheus/client_golang instead.
//
// Format reference: one `# HELP`, one `# TYPE`, then the samples, body ending
// in a newline. Prometheus rejects a body that does not.

const CONTENT_TYPE = 'text/plain; version=0.0.4; charset=utf-8';

// Render a value the way the exposition format expects. Number#toString gives
// exponential notation above 1e21, which Prometheus will not parse — a byte
// counter on a long-lived proxy can reach that.
function formatValue(v) {
  if (!Number.isFinite(v)) return String(v);
  if (Number.isInteger(v) && Math.abs(v) < 1e21) return v.toString();
  if (Math.abs(v) >= 1e21) return BigInt(Math.round(v)).toString();
  return v.toString();
}

class Metrics {
  constructor() {
    this.wsActive = 0;
    this.wsTotal = 0;
    this.wsRejectedTotal = 0;
    this.proxyErrorsTotal = 0;
    this.bytes = { to_browser: 0, to_gateway: 0 };
  }

  wsConnected() {
    this.wsActive++;
    this.wsTotal++;
  }

  // cleanup() in server.js is idempotent but reachable from both the ws and tcp
  // close handlers, so clamp rather than trusting the call to be balanced — a
  // negative gauge would be a nonsense reading on the dashboard.
  wsDisconnected() {
    this.wsActive = Math.max(0, this.wsActive - 1);
  }

  wsRejected() {
    this.wsRejectedTotal++;
  }

  proxyError() {
    this.proxyErrorsTotal++;
  }

  bytesToBrowser(n) {
    if (n > 0) this.bytes.to_browser += n;
  }

  bytesToGateway(n) {
    if (n > 0) this.bytes.to_gateway += n;
  }

  render() {
    const out = [];
    const emit = (name, type, help, samples) => {
      out.push(`# HELP ${name} ${help}`);
      out.push(`# TYPE ${name} ${type}`);
      for (const [labels, value] of samples) {
        out.push(`${name}${labels} ${formatValue(value)}`);
      }
    };

    emit('telixweb_ws_connections_active', 'gauge',
      'Browser terminals currently connected.',
      [['', this.wsActive]]);

    emit('telixweb_ws_connections_total', 'counter',
      'Browser terminal connections accepted since start.',
      [['', this.wsTotal]]);

    emit('telixweb_ws_rejected_total', 'counter',
      'WebSocket connections refused by the per-IP limit.',
      [['', this.wsRejectedTotal]]);

    emit('telixweb_proxy_errors_total', 'counter',
      'TCP errors reaching the Telix gateway.',
      [['', this.proxyErrorsTotal]]);

    emit('telixweb_bytes_total', 'counter',
      'Bytes proxied between browser and gateway, by direction.',
      [
        ['{direction="to_browser"}', this.bytes.to_browser],
        ['{direction="to_gateway"}', this.bytes.to_gateway],
      ]);

    emit('telixweb_process_uptime_seconds', 'gauge',
      'Seconds since this process started.',
      [['', process.uptime()]]);

    emit('telixweb_process_resident_memory_bytes', 'gauge',
      'Resident set size in bytes.',
      [['', process.memoryUsage().rss]]);

    return out.join('\n') + '\n';
  }
}

module.exports = { Metrics, CONTENT_TYPE };

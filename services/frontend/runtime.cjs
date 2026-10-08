// Single-process Next.js runtime with optional TLS and graceful draining.
const fs = require("node:fs");
const http = require("node:http");
const https = require("node:https");
const tls = require("node:tls");
const next = require("next");
const { instrument, metricsHandler } = require("./metrics.cjs");

const { config } = require("./.next/required-server-files.json");
process.env.__NEXT_PRIVATE_STANDALONE_CONFIG = JSON.stringify(config);
const cert = process.env.TLS_CERT_FILE;
const key = process.env.TLS_KEY_FILE;
if (Boolean(cert) !== Boolean(key)) throw new Error("Configure both TLS_CERT_FILE and TLS_KEY_FILE");
const credentials = () => ({ cert: fs.readFileSync(cert), key: fs.readFileSync(key) });

async function main() {
  const app = next({ dev: false, dir: __dirname, port: 3000 });
  await app.prepare();
  const handler = instrument(app.getRequestHandler());
  const tlsOptions = cert ? {
    ...credentials(),
    minVersion: "TLSv1.2",
    // Reload projected cert-manager Secrets for new client handshakes.
    SNICallback(_name, callback) {
      fs.readFile(cert, (certError, certData) => {
        if (certError) return callback(certError);
        fs.readFile(key, (keyError, keyData) => {
          if (keyError) return callback(keyError);
          callback(null, tls.createSecureContext({ cert: certData, key: keyData }));
        });
      });
    },
  } : null;
  const server = cert ? https.createServer(tlsOptions, handler) : http.createServer(handler);
  const metrics = http.createServer(metricsHandler);
  // Omitting the host listens dual-stack, or IPv4 only where IPv6 is disabled.
  metrics.listen(9090);
  server.listen(3000, () => {
    console.info(`Lolcatz available at ${process.env.NEXTAUTH_URL || `${cert ? "https" : "http"}://localhost:3000`}`);
  });
  let closing = false;
  const shutdown = () => {
    if (closing) return;
    closing = true;
    const deadline = setTimeout(() => process.exit(1), 25000);
    deadline.unref();
    metrics.close();
    server.close(async () => {
      await app.close();
      clearTimeout(deadline);
      process.exit(0);
    });
  };
  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);
}

void main();

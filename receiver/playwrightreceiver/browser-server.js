// Starts a Playwright browser server that exposes a WebSocket endpoint.
// The receiver creates its own monitoring page after connecting.
//
// Usage: node browser-server.js [port]
// Prints the WebSocket endpoint URL to stdout, then keeps running.

const { chromium } = require("playwright");

const port = parseInt(process.argv[2] || "0", 10);

(async () => {
  const server = await chromium.launchServer({
    headless: true,
    port: port,
  });

  const wsEndpoint = server.wsEndpoint();
  console.log(`WS_ENDPOINT=${wsEndpoint}`);

  process.on("SIGTERM", async () => {
    await server.close();
    process.exit(0);
  });
  process.on("SIGINT", async () => {
    await server.close();
    process.exit(0);
  });
})();

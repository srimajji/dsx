'use strict';

const http = require('node:http');
const fs = require('node:fs');
const net = require('node:net');

function acceptsConnection(port) {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host: '127.0.0.1', port });
    const finish = (ready) => {
      socket.destroy();
      resolve(ready);
    };
    socket.setTimeout(1000, () => finish(false));
    socket.once('connect', () => finish(true));
    socket.once('error', () => finish(false));
  });
}

const server = http.createServer(async (_request, response) => {
  const [mariaDBReady, redisReady] = await Promise.all([
    acceptsConnection(3306),
    acceptsConnection(6379),
  ]);
  if (!mariaDBReady || !redisReady) {
    response.writeHead(503, { 'content-type': 'text/plain' });
    response.end(`mariadb=${mariaDBReady ? 'ready' : 'unavailable'} redis=${redisReady ? 'ready' : 'unavailable'}\n`);
    return;
  }
  response.writeHead(200, { 'content-type': 'text/plain' });
  response.end('mariadb=ready redis=ready\n');
});

async function start() {
  const pending = [
    '/tmp/dsx-composite-mariadb-not-ready',
    '/tmp/dsx-composite-redis-not-ready',
  ].filter((path) => fs.existsSync(path));
  if (pending.length !== 0) {
    process.stderr.write(`dependencies launched out of order: ${pending.join(',')}\n`);
    process.exitCode = 1;
    return;
  }
  const dependencies = await Promise.all([
    acceptsConnection(3306),
    acceptsConnection(6379),
  ]);
  if (dependencies.some((ready) => !ready)) {
    process.stderr.write('dependencies were not ready before app launch\n');
    process.exitCode = 1;
    return;
  }
  server.listen(3000, '127.0.0.1', () => {
    fs.unlinkSync('/tmp/dsx-composite-app-not-ready');
  });
}

start().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});

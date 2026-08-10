import { networkInterfaces } from "node:os";
import { spawn } from "node:child_process";

function isPrivateIPv4(value) {
  const octets = value.split(".").map(Number);
  if (octets.length !== 4 || octets.some((octet) => !Number.isInteger(octet) || octet < 0 || octet > 255)) return false;
  return octets[0] === 10 ||
    (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
    (octets[0] === 192 && octets[1] === 168);
}

const addresses = new Set();
for (const entries of Object.values(networkInterfaces())) {
  for (const entry of entries ?? []) {
    if (entry.family === "IPv4" && !entry.internal && isPrivateIPv4(entry.address)) addresses.add(entry.address);
  }
}
if (addresses.size !== 1) {
  throw new Error(`expected exactly one private IPv4 address, found ${addresses.size}`);
}
const allowedAuthority = `${[...addresses][0]}:8931`;
const child = spawn("/usr/bin/node", [
  "/app/node_modules/@playwright/mcp/cli.js",
  "--headless", "--browser", "chromium", "--isolated",
  "--port", "8931", "--host", "0.0.0.0",
  "--allowed-hosts", allowedAuthority,
], { stdio: "inherit" });
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT"]) {
  process.on(signal, () => child.kill(signal));
}
child.on("error", (error) => {
  console.error(error.message);
  process.exit(1);
});
child.on("exit", (code) => process.exit(code ?? 1));

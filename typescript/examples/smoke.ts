// One real round trip on a cloud: create, run, stream, list, reattach, destroy.
// Usage: smoke.ts aws|gcp
// The TTL is 30 minutes; only AWS terminates the machine by itself if this is interrupted.
import { once } from "node:events";

const opts = { keyPath: process.env.NEPHO_KEY };
const p = process.argv[2] === "aws"
  ? new (await import("../src/aws.js")).AwsProvider(opts)
  : new (await import("../src/gcp.js")).GcpProvider(opts);
let t = Date.now();
let m = await p.create({
  os: "ubuntu-24.04", ttlSeconds: 1800,
  setup: { packages: ["jq"], files: { "/etc/nepho-test": "hello\n" }, script: "echo done > /root/marker" },
});
console.log("created", m.id, `${Math.round((Date.now() - t) / 1000)}s`);
try {
  const r = await m.run("uname -a; cat /etc/nepho-test; jq --version; sudo cat /root/marker; whoami");
  console.log(r.code, r.stdout, r.stderr);
  const pr = await m.execute(["cat"]);
  pr.stdin.write("stream ok\n");
  const [chunk] = await once(pr.stdout, "data");
  console.log("echo:", String(chunk).trim());
  pr.stdin.end();
  console.log("cat exit", await pr.wait());
  console.log("as root:", (await m.run("id -un", { user: "root" })).stdout.trim());
  await m.detach();
  for (const i of await p.list()) console.log("list:", i.id, i.state, i.address);
  m = await p.attach(m.id);
  console.log("reattached:", (await m.run("uptime")).stdout.trim());
} finally {
  t = Date.now();
  await m.destroy();
  console.log("destroyed", `${Math.round((Date.now() - t) / 1000)}s`);
  for (const i of await p.list()) console.log("after:", i.id, i.state);
}

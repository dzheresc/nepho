import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { AwsProvider } from "../src/aws.js";

const group = (gid: string, tags: Record<string, string>) =>
  `<item><groupId>${gid}</groupId><tagSet>` + Object.entries(tags)
    .map(([k, v]) => `<item><key>${k}</key><value>${v}</value></item>`).join("") + "</tagSet></item>";
const GROUPS: Record<string, string> = {
  "m-old": group("sg-m", { "nepho-managed": "true", "nepho-id": "m-old", "nepho-expires": "1" }),
  "s-old": group("sg-s", {
    "nepho-managed": "true", "nepho-id": "s-old", "nepho-expires": "1", "nepho-kind": "postgres",
  }),
};

test("aws: reap removes an expired machine's group and leaves a service's group alone", async () => {
  const deleted: (string | null)[] = [];
  const server = createServer(async (req, res) => {
    let body = "";
    for await (const chunk of req) body += chunk;
    const q = new URLSearchParams(body);
    const action = q.get("Action");
    let inner = "";
    if (action === "DescribeSecurityGroups") {
      const id = q.get("Filter.1.Name") === "tag:nepho-id" ? q.get("Filter.1.Value.1")! : null;
      const items = id ? GROUPS[id] ?? "" : Object.values(GROUPS).join("");
      inner = `<securityGroupInfo>${items}</securityGroupInfo>`;
    } else if (action === "DeleteSecurityGroup") deleted.push(q.get("GroupId"));
    res.setHeader("Content-Type", "text/xml");
    res.end(`<${action}Response>${inner}</${action}Response>`);
  });
  await new Promise<void>((done) => server.listen(0, "127.0.0.1", done));
  const dir = mkdtempSync(join(tmpdir(), "nepho-"));
  Object.assign(process.env, {
    AWS_ENDPOINT_URL: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    AWS_ACCESS_KEY_ID: "test", AWS_SECRET_ACCESS_KEY: "test",
  });
  try {
    const p = new AwsProvider({ region: "us-east-1", keyPath: join(dir, "id_rsa") });
    assert.equal(await p.reap(), 1);
    assert.deepEqual(deleted, ["sg-m"]);
  } finally {
    server.closeAllConnections();
    server.close();
    rmSync(dir, { recursive: true });
  }
});

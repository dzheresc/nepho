# nephodrome

One API for machines on Docker, AWS, GCP and Azure. The package is called
`nepho` and exists three times with the same behaviour: `python/`,
`typescript/` and `go/`. Linux guests only.

## What it does

- `create(spec)` makes a machine with the requested OS, installs the
  prerequisites, and returns when commands can be run.
- `execute(argv)` starts a command and gives live text streams: stdin, stdout,
  stderr, plus the exit code, the remote PID and `kill`.
- `list()` returns every machine the backend still knows, in any state
  (`pending`, `running`, `stopped`, `terminated`).
- `attach(id)` gives a handle to an existing machine from any process.
  `detach()` drops the connection and leaves the machine and its processes
  running. Only `destroy()` removes a machine.
- `reap()` destroys machines whose TTL (default 4 hours) has passed.

## Usage

Python:

```python
from nepho import MachineSpec, Setup
from nepho.docker import DockerProvider      # or nepho.aws / nepho.gcp / nepho.azure

provider = DockerProvider()
m = await provider.create(MachineSpec(os="ubuntu-24.04", setup=Setup(packages=["git"])))
p = await m.execute(["cat"])
p.stdin.write("hello\n")
print(await p.stdout.readline())
p.stdin.close()
print(await p.wait())
await m.detach()

m = await provider.attach(m.id)              # later, possibly from another process
print((await m.run("uname -a")).stdout)
await m.destroy()
```

TypeScript:

```ts
import { DockerProvider } from "nepho/docker";

const provider = new DockerProvider();
const m = await provider.create({ os: "ubuntu-24.04", setup: { packages: ["git"] } });
const p = await m.execute(["cat"]);
p.stdin.end("hello\n");
for await (const chunk of p.stdout) process.stdout.write(chunk);
console.log(await p.wait());
await m.destroy();
```

Go:

```go
provider, _ := docker.New(docker.Options{})
m, _ := provider.Create(ctx, nepho.Spec{OS: "ubuntu-24.04", Setup: nepho.Setup{Packages: []string{"git"}}})
p, _ := m.Execute(ctx, []string{"cat"}, nil)
fmt.Fprintln(p.Stdin, "hello")
p.Stdin.Close()
io.Copy(os.Stdout, p.Stdout)
code, _ := p.Wait()
m.Destroy(ctx)
```

A command given as a list runs as is. A string (`nepho.Sh("...")` in Go) runs
through `sh -c`. Options: `env`, `cwd`, `user`, `pty`.

## How it works

| Backend | Machine | Commands run over |
|---|---|---|
| Docker | container running `sleep <ttl>` | Docker exec API |
| AWS | EC2 instance + security group | SSH |
| GCP | Compute Engine instance + firewall rule | SSH |
| Azure | resource group with VM, network, public IP | SSH |

- Prerequisites are applied through `execute` itself, as root, in this order:
  apt packages, files, script. A failure raises `SetupError` with the output
  and destroys the half-made machine.
- Every resource carries the labels `nepho-managed`, `nepho-id` and
  `nepho-expires`. That is how `list`, `attach` and `reap` find machines, so
  the three implementations can manage each other's machines.
- The clouds share one SSH key, `~/.config/nepho/id_rsa`, created on first
  use. Port 22 is opened only to the caller's public IP (override with the
  `ssh_cidr` option).
- Supported `os` values: `ubuntu-24.04`, `ubuntu-22.04`, `debian-12`.
- The default size is 1 CPU and 1 GB of memory, which gives the smallest
  instance type in each cloud's table: `t3.micro`, `e2-micro` and, on Azure,
  `Standard_D2als_v6` (2 CPUs / 4 GB, several times the price of the other
  two). Ask for more with `cpus` and `memory_gb`; the first large-enough type
  is used.

## Limits

- AWS, GCP and Azure have been run against real accounts from all three
  languages (Ubuntu 24.04: create with prerequisites, execute, list, attach,
  destroy). Not yet exercised there: `reap()`, Debian 12, Ubuntu 22.04, and
  TTL self-termination on AWS.
- EC2 keeps listing a terminated instance for about an hour. On GCP a
  destroyed machine disappears from `list()` at once.
- On GCP the stock firewall rule `default-allow-ssh` opens port 22 to
  everyone, so the per-machine rule limits nothing until that rule is deleted.
- A Docker "machine" is a container: no systemd, shared kernel.
- SSH host keys of new VMs are not verified.
- Only AWS and Docker stop a machine by themselves at the TTL. On GCP and
  Azure something has to call `reap()`.
- Output is buffered until read. For large outputs read the streams or use
  `run()`.
- With `pty`, stderr is merged into stdout and closing stdin sends Ctrl-D.
- The Go module path `example.com/nepho` is a placeholder.

## Tests

The same 24 conformance checks exist in each language. They need only Docker.
Run one suite at a time: they share the Docker daemon and count each other's
machines.

```sh
cd python && uv venv && uv pip install -e '.[all,test]' && .venv/bin/pytest
./node.sh sh -c 'npm install && npm test'    # Node runs in a container
./go.sh go test ./...                        # Go runs in a container
```

`node.sh` and `go.sh` pin the toolchain container to CPUs 14-27 / NUMA node 1;
override with `NEPHO_CPUS` and `NEPHO_MEMS`.

## Running against AWS

Credentials come from the SDK default chain (`~/.aws/config` and
`~/.aws/credentials`). The IAM user needs: `ec2:DescribeImages`,
`ec2:DescribeInstances`, `ec2:DescribeSecurityGroups`, `ec2:RunInstances`,
`ec2:CreateSecurityGroup`, `ec2:CreateTags`, `ec2:TerminateInstances`,
`ec2:DeleteSecurityGroup`, `ec2:AuthorizeSecurityGroupIngress`, and
`ssm:GetParameter` on `arn:aws:ssm:*::parameter/aws/service/*`. The last three
EC2 actions can be limited to resources tagged `nepho-managed=true`.

## Running against GCP

Credentials are Application Default Credentials; the project is taken from
them. With a service account key: `GOOGLE_APPLICATION_CREDENTIALS=<key.json>`.
The roles Compute Instance Admin (v1) and Compute Security Admin are enough.
The Compute Engine API must be enabled and the `default` network must exist.
The zone is a provider option, default `us-central1-a`.

## Running against Azure

Credentials come from `DefaultAzureCredential`. With an app registration set
`AZURE_SUBSCRIPTION_ID`, `AZURE_TENANT_ID`, `AZURE_CLIENT_ID` and
`AZURE_CLIENT_SECRET`. The app needs the Contributor role on the subscription,
because each machine gets its own resource group. The resource providers
`Microsoft.Compute` and `Microsoft.Network` must be registered. The location
is a provider option, default `eastus`.

The size table uses the Dalsv6 and Dasv6 families. The cheaper B-series is
left out: on the test subscription the old B-series was capacity-restricted and
the v2 B-series had a core quota of 0 that could not be raised. Where it is
available, pass it with the `vm_size` option (for example `Standard_B2ts_v2`).

Azure creates a resource group `NetworkWatcherRG` by itself; it is not a nepho
resource.

## Smoke examples

Each example launches one small instance for a few minutes and destroys it.
The argument is `aws`, `gcp` or `azure`, optionally followed by an instance
size that overrides the default:

```sh
cd python && .venv/bin/python examples/smoke.py aws
./node.sh npx tsx examples/smoke.ts gcp
./go.sh go run ./examples/smoke azure Standard_D2as_v6
```

Outside the containers, load the Azure variables first:
`set -a; . ~/.config/nepho/azure.env; set +a`.

`node.sh` and `go.sh` mount `~/.aws` (read-only) and `~/.config/nepho` into
the container. If `~/.config/nepho/gcp.json` exists they use it as the GCP
service account key, and `~/.config/nepho/azure.env` is loaded as environment
variables.

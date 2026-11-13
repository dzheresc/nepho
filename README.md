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
  instance type of each cloud (`t3.micro`, `e2-micro`, `Standard_B1s`). Ask
  for more with `cpus` and `memory_gb`; the first large-enough type is used.

## Limits

- AWS and GCP have been run against real accounts from all three languages
  (Ubuntu 24.04: create with prerequisites, execute, list, attach, destroy).
  Not yet exercised there: `reap()`, Debian 12, Ubuntu 22.04, and TTL
  self-termination on AWS.
- Azure has never been run against a real account. It compiles and
  type-checks against the current SDKs, and the SSH transport it shares with
  the other clouds is tested against an sshd in a container.
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

## Smoke examples

Each example launches one small instance for a few minutes and destroys it.
The argument is `aws` or `gcp`:

```sh
cd python && .venv/bin/python examples/smoke.py aws
./node.sh npx tsx examples/smoke.ts aws
./go.sh go run ./examples/smoke aws
```

`node.sh` and `go.sh` mount `~/.aws` (read-only) and `~/.config/nepho` into
the container. If `~/.config/nepho/gcp.json` exists they use it as the GCP
service account key.

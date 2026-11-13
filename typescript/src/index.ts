// One API for machines on Docker, AWS, GCP and Azure.
// Providers live in their own entry points so that only the SDK you use is loaded:
//   import { DockerProvider } from "nepho/docker";
//   import { AwsProvider } from "nepho/aws";
//   import { GcpProvider } from "nepho/gcp";
//   import { AzureProvider } from "nepho/azure";
export {
  CommandError, Machine, MachineError, Provider, ProvisionError, SetupError, TransportError,
} from "./core.js";
export type {
  ExecOptions, Info, MachineSpec, Process, Result, Setup, State, Transport,
} from "./core.js";

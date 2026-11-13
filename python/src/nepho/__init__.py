"""One API for machines on Docker, AWS, GCP and Azure.

Providers live in their own modules so that only the SDK you use is imported:

    from nepho.docker import DockerProvider
    from nepho.aws import AwsProvider
    from nepho.gcp import GcpProvider
    from nepho.azure import AzureProvider
"""
from .core import (PENDING, RUNNING, STOPPED, TERMINATED, CommandError, Machine, MachineError,
                   MachineSpec, Process, Provider, ProvisionError, Result, Setup, SetupError,
                   TextReader, TextWriter, TransportError)

__all__ = ["PENDING", "RUNNING", "STOPPED", "TERMINATED", "CommandError", "Machine",
           "MachineError", "MachineSpec", "Process", "Provider", "ProvisionError", "Result",
           "Setup", "SetupError", "TextReader", "TextWriter", "TransportError"]

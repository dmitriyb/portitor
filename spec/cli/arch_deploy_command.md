# DeployCommand

The `portitor deploy` subcommand: the thin Go front-end of the delivery module's Deploy
component (`spec/delivery/arch_deploy.md`), which owns the deploy config, the image and the
sequence.

## Responsibilities

- Register as a provisioning-group cobra subcommand (`newDeployCmd()` in
  `cmd/portitor/commands.go`), with universal `--help`.
- Take `--config <file>` (required) and no positional arguments.
- Run the deploy and stream one line per step to stderr, so a refusal names the step and its
  reason.
- Print nothing to stdout except, on success, the deployed container ID and image tag.

## Exit codes

0 when the deploy reached its commit step; 1 on any refusal or failure (the deploy has already
restored the previous gate where the sequence says so); 2 on a usage error.

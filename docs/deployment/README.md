# Deployment

The active deployment is the [native NixOS service](nixos.md). The monorepo
builds the gateway and native Headroom plugin with Buck, then supplies that
package to the NixOS modules in this fork.

Cloudflare Workers and Containers are not deployment targets. Bifrost performs
its normal database migrations during startup with the dedicated database owner.
There is no separate migration executable or systemd migration unit.

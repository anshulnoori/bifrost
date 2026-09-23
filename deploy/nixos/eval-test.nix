# nix eval --impure --json --file deploy/nixos/eval-test.nix
let
  flake = builtins.getFlake "git+file://${toString ../..}";
  evaluate = public: flake.inputs.nixpkgs.lib.nixosSystem {
    system = "aarch64-linux";
    modules = [
      flake.nixosModules.deployment
      ({ pkgs, lib, ... }: {
        nixpkgs.hostPlatform = "aarch64-linux";
        # Evaluate topology without building a binary on this architecture.
        services.bifrost.package = lib.mkForce pkgs.hello;
        services.bifrostDeployment = { enable = true; publicInference = public; };
        system.stateVersion = "26.05";
        boot.loader.grub.enable = false;
        fileSystems."/" = { device = "/dev/disk/by-label/nixos"; fsType = "ext4"; };
      })
    ];
  };
  private = (evaluate false).config;
  public = (evaluate true).config;
in
assert private.services.bifrost.host == "127.0.0.1";
assert private.services.redis.servers.bifrost.bind == "127.0.0.1";
assert private.services.redis.servers.bifrost.settings.maxmemory == "2gb";
assert private.services.redis.servers.bifrost.settings.save == "\"\"";
assert private.systemd.services.bifrost-migrate.wantedBy == [];
assert private.services.bifrost.settings.client.enforce_auth_on_inference;
assert private.services.bifrost.settings.governance.auth_config.is_enabled;
assert private.services.bifrost.settings.config_store.config.ssl_mode == "verify-full";
assert private.networking.firewall.allowedTCPPorts == [];
assert !(private.systemd.services ? bifrost-inference-funnel);
assert builtins.match ".*--https=443 http://127.0.0.1:8081" public.systemd.services.bifrost-inference-funnel.serviceConfig.ExecStart != null;
assert builtins.match ".*--https=8443 http://127.0.0.1:8082" public.systemd.services.bifrost-admin-serve.serviceConfig.ExecStart != null;
assert builtins.all (a: a.assertion) private.assertions;
{
  evaluatedSystem = private.system.build.toplevel.drvPath;
  architecture = private.nixpkgs.hostPlatform.system;
  checks = "loopback, auth, TLS, firewall, memory and separate Serve/Funnel services passed";
}

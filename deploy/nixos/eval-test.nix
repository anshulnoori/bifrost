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
        services.bifrostDeployment = {
          enable = true;
          publicInference = public;
          headroomEndpoint = if public then "https://synthetic-headroom.modal.run" else null;
        };
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
assert !private.virtualisation.podman.enable;
assert private.virtualisation.oci-containers.containers == {};
assert private.systemd.services.bifrost-valkey.serviceConfig.DynamicUser;
assert private.systemd.services.bifrost-valkey.serviceConfig.RuntimeDirectoryMode == "0700";
assert private.systemd.services.bifrost-valkey.serviceConfig.LoadCredential == [ "valkey-password:/run/secrets/valkey-password" ];
assert builtins.elem "bifrost-valkey.service" private.systemd.services.bifrost.requires;
assert private.services.bifrost.settings.vector_store.config.addr == "127.0.0.1:6379";
assert (builtins.head private.services.bifrost.settings.plugins).config.scope_by_virtual_key;
assert (builtins.head private.services.bifrost.settings.plugins).config.dimension == 1;
assert (builtins.head public.services.bifrost.settings.plugins).config.dimension == 384;
assert (builtins.head public.services.bifrost.settings.plugins).config.provider == "headroom_embeddings";
assert public.services.bifrost.settings.providers.headroom_embeddings.custom_provider_config.allowed_requests == { embedding = true; };
assert (builtins.head public.services.bifrost.settings.providers.headroom_embeddings.keys).value == "env.HEADROOM_METRICS_TOKEN";
assert !(builtins.elemAt private.services.bifrost.settings.plugins 1).config.enabled;
assert (builtins.elemAt public.services.bifrost.settings.plugins 1).config.enabled;
assert (builtins.elemAt public.services.bifrost.settings.plugins 1).config.timeout_ms == 500;
assert private.systemd.services.bifrost-migrate.wantedBy == [];
assert private.services.bifrost.settings.client.enforce_auth_on_inference;
assert private.services.bifrost.settings.governance.auth_config.is_enabled;
assert private.services.bifrost.settings.config_store.config.ssl_mode == "verify-full";
assert private.networking.firewall.allowedTCPPorts == [];
assert !(private.systemd.services ? bifrost-inference-funnel);
assert builtins.match ".*serve --service=svc:ai --https=443 http://127.0.0.1:8081" private.systemd.services.bifrost-inference-serve.serviceConfig.ExecStart != null;
assert builtins.match ".*serve --service=svc:ai --https=8443 http://127.0.0.1:8082" private.systemd.services.bifrost-admin-serve.serviceConfig.ExecStart != null;
assert builtins.match ".*serve --service=svc:ai --https=8443 off" private.systemd.services.bifrost-admin-serve.serviceConfig.ExecStop != null;
assert builtins.match ".*--https=443 http://127.0.0.1:8081" public.systemd.services.bifrost-inference-funnel.serviceConfig.ExecStart != null;
assert builtins.match ".*--https=8443 http://127.0.0.1:8082" public.systemd.services.bifrost-admin-serve.serviceConfig.ExecStart != null;
assert builtins.all (a: a.assertion) private.assertions;
{
  evaluatedSystem = private.system.build.toplevel.drvPath;
  architecture = private.nixpkgs.hostPlatform.system;
  checks = "loopback, auth, TLS, firewall, memory and separate Serve/Funnel services passed";
}

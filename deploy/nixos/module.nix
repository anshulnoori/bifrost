{ config, lib, pkgs, ... }:
let
  cfg = config.services.bifrostDeployment;
  db = {
    host = "env.NEON_HOST";
    port = "5432";
    user = "env.NEON_USER";
    password = "env.NEON_PASSWORD";
    db_name = "env.NEON_DATABASE";
    ssl_mode = "verify-full";
    max_open_conns = 5;
    max_idle_conns = 1;
    conn_max_lifetime = "5m";
    conn_max_idle_time = "30s";
  };
in {
  options.services.bifrostDeployment = {
    enable = lib.mkEnableOption "private Bifrost with allowlisted Funnel inference";
    environmentFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/bifrost.env";
      description = "Absolute runtime secret file; never a Nix store path.";
    };
    redisPasswordFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/valkey-password";
      description = "Runtime file containing the Valkey password.";
    };
    migrationEnvironmentFile = lib.mkOption {
      type = lib.types.str;
      default = "/run/secrets/bifrost-migration.env";
      description = "Separate owner-only direct Neon URL for the manual migration unit.";
    };
    publicInference = lib.mkEnableOption "public Funnel after private validation";
  };

  config = lib.mkIf cfg.enable {
    assertions = map (path: {
      assertion = lib.hasPrefix "/" path && !(lib.hasPrefix "/nix/store/" path);
      message = "Bifrost deployment secrets must be runtime absolute paths outside /nix/store.";
    }) [ cfg.environmentFile cfg.redisPasswordFile cfg.migrationEnvironmentFile ];

    nix.settings.experimental-features = [ "nix-command" "flakes" ];
    services.tailscale.enable = true;
    # No public inbound application ports, including SSH. Use tailnet SSH transport.
    services.openssh.openFirewall = false;
    networking.firewall.interfaces.tailscale0.allowedTCPPorts = [ 22 ];

    services.bifrost = {
      enable = true;
      host = "127.0.0.1";
      port = 8080;
      openFirewall = false;
      environmentFile = cfg.environmentFile;
      logLevel = "error";
      settings = {
        encryption_key = "env.BIFROST_ENCRYPTION_KEY";
        client = {
          enforce_auth_on_inference = true;
          enable_logging = true;
          disable_content_logging = true;
          allow_per_request_content_storage_override = false;
          allow_per_request_raw_override = false;
        };
        config_store = { enabled = true; type = "postgres"; config = db; };
        logs_store = {
          enabled = true;
          type = "postgres";
          config = db // { matview_refresh_interval = "off"; };
          retention_days = 7;
        };
        governance.auth_config = {
          is_enabled = true;
          admin_username = "owner";
          admin_password = "env.BIFROST_ADMIN_PASSWORD";
        };
        # GPU/private Modal and Search compatibility are release gates, not defaults.
        plugins = [ {
          name = "headroom";
          path = "${config.services.bifrost.package}/lib/headroom.so";
          enabled = true;
          placement = "post_builtin";
          config = { enabled = false; ccr = false; };
        } ];
      };
    };
    systemd.services.bifrost = {
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      preStart = lib.mkBefore ''
        for name in NEON_HOST NEON_USER NEON_PASSWORD NEON_DATABASE BIFROST_ENCRYPTION_KEY BIFROST_ADMIN_PASSWORD; do
          if [ -z "''${!name:-}" ]; then echo "Required deployment setting missing" >&2; exit 1; fi
        done
        [[ "$NEON_HOST" == *-pooler.*.neon.tech ]] || exit 1
        [[ ''${#BIFROST_ENCRYPTION_KEY} -ge 32 && ''${#BIFROST_ADMIN_PASSWORD} -ge 32 ]] || exit 1
      '';
      serviceConfig = {
        Restart = "on-failure";
        RestartSec = 5;
        TimeoutStopSec = 45;
        MemoryHigh = "4G";
        MemoryMax = "6G";
        LimitCORE = 0;
        # Upstream errors can contain connection strings. Use DB metadata logs.
        StandardOutput = "null";
        StandardError = "null";
      };
    };

    # Never runs at boot or as a dependency of the runtime service.
    systemd.services.bifrost-migrate = {
      description = "Owner-operated Bifrost schema migration";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${config.services.bifrost.package}/bin/bifrost-migrate --migrate";
        EnvironmentFile = cfg.migrationEnvironmentFile;
        DynamicUser = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        LimitCORE = 0;
        TimeoutStartSec = 660;
        StandardOutput = "null";
        StandardError = "null";
      };
    };

    services.redis = {
      package = pkgs.valkey;
      servers.bifrost = {
        enable = true;
        bind = "127.0.0.1";
        port = 6379;
        openFirewall = false;
        requirePassFile = cfg.redisPasswordFile;
        # Disposable cache only. Durable learned memory must live elsewhere.
        save = [];
        appendOnly = false;
        settings = {
          maxmemory = "2gb";
          maxmemory-policy = "allkeys-lfu";
        };
      };
    };
    systemd.services.redis-bifrost.serviceConfig.MemoryMax = "3G";

    services.caddy = {
      enable = true;
      enableReload = false; # The Caddy admin API is intentionally disabled.
      openFirewall = false;
      configFile = ./Caddyfile;
    };
    systemd.services.caddy.serviceConfig.LimitCORE = 0;

    systemd.services.bifrost-admin-serve = {
      description = "Tailnet-only Bifrost administration";
      wantedBy = [ "multi-user.target" ];
      after = [ "tailscaled.service" "caddy.service" ];
      requires = [ "tailscaled.service" "caddy.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.tailscale}/bin/tailscale serve --bg --https=8443 http://127.0.0.1:8082";
        ExecStop = "${pkgs.tailscale}/bin/tailscale serve --https=8443 off";
      };
    };
    systemd.services.bifrost-inference-funnel = lib.mkIf cfg.publicInference {
      description = "Public inference only (never the Bifrost admin listener)";
      wantedBy = [ "multi-user.target" ];
      after = [ "tailscaled.service" "caddy.service" "bifrost.service" ];
      requires = [ "tailscaled.service" "caddy.service" "bifrost.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.tailscale}/bin/tailscale funnel --bg --https=443 http://127.0.0.1:8081";
        ExecStop = "${pkgs.tailscale}/bin/tailscale funnel --https=443 off";
      };
    };
  };
}

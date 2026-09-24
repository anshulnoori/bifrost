{ config, lib, pkgs, ... }:
let
  cfg = config.services.bifrostDeployment;
  valkeyImage = "docker.io/valkey/valkey-bundle@" + {
    aarch64-linux = "sha256:cc16e0c672ffdfdbee4581e146d41086f408468489e21b8625f877a332b998ba";
    x86_64-linux = "sha256:203692cbb7d59887cd7723f88cefa0c470d74037e3f82024b17cfac31345d4f5";
  }.${pkgs.stdenv.hostPlatform.system};
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
    headroomEndpoint = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Authenticated Modal HTTPS origin, set only after CPU/CUDA benchmark acceptance.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = map (path: {
      assertion = lib.hasPrefix "/" path && !(lib.hasPrefix "/nix/store/" path);
      message = "Bifrost deployment secrets must be runtime absolute paths outside /nix/store.";
    }) [ cfg.environmentFile cfg.redisPasswordFile cfg.migrationEnvironmentFile ] ++ [ {
      assertion = cfg.headroomEndpoint == null || builtins.match "https://[a-zA-Z0-9-]+\\.modal\\.run" cfg.headroomEndpoint != null;
      message = "Headroom requires a Modal HTTPS origin without credentials, path or query.";
    } ];

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
        vector_store = {
          enabled = true;
          type = "redis";
          config = { addr = "127.0.0.1:6379"; password = "env.VALKEY_PASSWORD"; };
        };
        # Only the plugin holds Modal credentials. The embedding provider reaches
        # its authenticated loopback facade using a SecretVar-backed key.
        providers = lib.optionalAttrs (cfg.headroomEndpoint != null) {
          headroom_embeddings = {
            keys = [ {
              name = "headroom-internal";
              value = "env.HEADROOM_METRICS_TOKEN";
              models = [ "headroom-minilm-v1" ];
              weight = 1;
            } ];
            custom_provider_config = {
              base_provider_type = "openai";
              is_key_less = false;
              allowed_requests = { embedding = true; };
            };
            network_config = {
              base_url = "http://127.0.0.1:9909";
              allow_private_network = true;
              default_request_timeout_in_seconds = 2;
              max_retries = 0;
            };
          };
        };
        plugins = [ {
          name = "semantic_cache";
          enabled = true;
          config = {
            provider = if cfg.headroomEndpoint == null then "" else "headroom_embeddings";
            embedding_model = "headroom-minilm-v1";
            dimension = if cfg.headroomEndpoint == null then 1 else 384;
            threshold = 0.98;
            ttl = "5m";
            default_cache_key = "deployment-v1";
            scope_by_virtual_key = true;
            vector_store_namespace = if cfg.headroomEndpoint == null then "BifrostScopedCacheV1" else "BifrostMiniLMCacheV1";
          };
        } {
          name = "headroom";
          path = "${config.services.bifrost.package}/lib/headroom.so";
          enabled = true;
          placement = "post_builtin";
          config = {
            enabled = cfg.headroomEndpoint != null;
            endpoint = if cfg.headroomEndpoint == null then "" else cfg.headroomEndpoint;
            scope = "gateway";
            ccr = false;
            token_env = "HEADROOM_PROXY_TOKEN";
            scope_key_env = "HEADROOM_SCOPE_KEY";
            modal_key_env = "HEADROOM_MODAL_KEY";
            modal_secret_env = "HEADROOM_MODAL_SECRET";
            failure_policy = "open";
            timeout_ms = 500;
            min_text_bytes = 4096;
            metrics_address = if cfg.headroomEndpoint == null then "" else "127.0.0.1:9909";
            metrics_token_env = "HEADROOM_METRICS_TOKEN";
            retention_seconds = 900;
          };
        } ];
      };
    };
    systemd.services.bifrost = {
      after = [ "network-online.target" "podman-bifrost-valkey.service" ];
      wants = [ "network-online.target" "podman-bifrost-valkey.service" ];
      preStart = lib.mkBefore (''
        for name in NEON_HOST NEON_USER NEON_PASSWORD NEON_DATABASE BIFROST_ENCRYPTION_KEY BIFROST_ADMIN_PASSWORD; do
          if [ -z "''${!name:-}" ]; then echo "Required deployment setting missing" >&2; exit 1; fi
        done
        [[ "$NEON_HOST" == *-pooler.*.neon.tech ]] || exit 1
        [[ ''${#BIFROST_ENCRYPTION_KEY} -ge 32 && ''${#BIFROST_ADMIN_PASSWORD} -ge 32 ]] || exit 1
      '' + lib.optionalString (cfg.headroomEndpoint != null) ''
        for name in HEADROOM_PROXY_TOKEN HEADROOM_SCOPE_KEY HEADROOM_METRICS_TOKEN HEADROOM_MODAL_KEY HEADROOM_MODAL_SECRET; do
          if [ -z "''${!name:-}" ]; then echo "Required Headroom setting missing" >&2; exit 1; fi
        done
        [[ ''${#HEADROOM_PROXY_TOKEN} -ge 32 && ''${#HEADROOM_SCOPE_KEY} -ge 32 && ''${#HEADROOM_METRICS_TOKEN} -ge 32 ]] || exit 1
      '');
      serviceConfig = {
        LoadCredential = [ "valkey-password:${cfg.redisPasswordFile}" ];
        ExecStart = lib.mkForce (pkgs.writeShellScript "bifrost-start" ''
          set -eu
          export VALKEY_PASSWORD="$(cat "$CREDENTIALS_DIRECTORY/valkey-password")"
          ready=false
          for attempt in {1..30}; do
            if VALKEYCLI_AUTH="$VALKEY_PASSWORD" ${pkgs.valkey}/bin/valkey-cli -e -h 127.0.0.1 FT._LIST >/dev/null 2>&1; then
              ready=true; break
            fi
            sleep 1
          done
          "$ready" || exit 1
          exec ${config.services.bifrost.package}/bin/bifrost-http \
            -host 127.0.0.1 -port 8080 -app-dir ${lib.escapeShellArg config.services.bifrost.stateDir} -log-level error
        '');
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

    # Official multi-architecture bundle: Valkey 9.1.1 + Search 1.2.1.
    # Only Search is loaded; no JSON, LDAP or Bloom modules are needed.
    virtualisation.oci-containers = {
      backend = "podman";
      containers.bifrost-valkey = {
        image = valkeyImage;
        user = "999:999";
        entrypoint = "valkey-server";
        cmd = [ "/etc/valkey.conf" ];
        volumes = [ "/run/bifrost-valkey/valkey.conf:/etc/valkey.conf:ro" ];
        extraOptions = [ "--network=host" "--read-only" "--cap-drop=ALL" "--memory=3g"
          "--security-opt=no-new-privileges" "--tmpfs=/data:uid=999,gid=999,mode=700" ];
      };
    };
    systemd.services.podman-bifrost-valkey = {
      preStart = lib.mkBefore ''
        set -eu
        password="$(cat "$CREDENTIALS_DIRECTORY/valkey-password")"
        [[ "$password" =~ ^[[:xdigit:]]{64}$ ]] || { echo "Valkey password must be 64 hex characters" >&2; exit 1; }
        umask 077
        {
          printf 'requirepass %s\n' "$password"
          cat <<'EOF'
        bind 127.0.0.1
        port 6379
        protected-mode yes
        loadmodule /usr/lib/valkey/libsearch.so
        save ""
        appendonly no
        maxmemory 2gb
        maxmemory-policy allkeys-lfu
        EOF
        } > /run/bifrost-valkey/valkey.conf
        chown 999:999 /run/bifrost-valkey/valkey.conf
      '';
      serviceConfig = {
        LoadCredential = [ "valkey-password:${cfg.redisPasswordFile}" ];
        RuntimeDirectory = "bifrost-valkey";
        RuntimeDirectoryMode = "0700";
        MemoryMax = "3G";
        LimitCORE = 0;
        StandardOutput = "null";
        StandardError = "null";
      };
    };

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

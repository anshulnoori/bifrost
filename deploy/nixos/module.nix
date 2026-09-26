{ config, lib, pkgs, ... }:
let
  cfg = config.services.bifrostDeployment;
  valkeySearch = pkgs.callPackage ../../nix/packages/valkey-search.nix {};
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
    headroomModalApp = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Private Modal app for Headroom compression.";
    };
    semanticCacheModalApp = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Private Modal app for MiniLM embeddings, independent of Headroom compression.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = map (path: {
      assertion = lib.hasPrefix "/" path && !(lib.hasPrefix "/nix/store/" path);
      message = "Bifrost deployment secrets must be runtime absolute paths outside /nix/store.";
    }) [ cfg.environmentFile cfg.redisPasswordFile cfg.migrationEnvironmentFile ] ++ [ {
      assertion = cfg.headroomModalApp == null || builtins.match "[a-zA-Z0-9][a-zA-Z0-9-]*" cfg.headroomModalApp != null;
      message = "Headroom requires a valid Modal app name.";
    } {
      assertion = cfg.semanticCacheModalApp == null || builtins.match "[a-zA-Z0-9][a-zA-Z0-9-]*" cfg.semanticCacheModalApp != null;
      message = "Semantic cache requires a valid Modal app name.";
    } {
      assertion = cfg.headroomModalApp == null || cfg.semanticCacheModalApp == null || cfg.headroomModalApp == cfg.semanticCacheModalApp;
      message = "Headroom compression and semantic cache must use the same Modal app.";
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
          allowed_origins = [
            "https://ai.mongoose-silverside.ts.net"
            "https://ai.mongoose-silverside.ts.net:8443"
          ];
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
        # Enable stable prompt prefixes for every configured provider. Per-provider
        # prompt_cache settings remain authoritative, including auto_inject = false.
        provider_defaults.prompt_cache = { auto_inject = true; };
        # Only the plugin holds Modal credentials. The embedding provider reaches
        # its authenticated loopback facade using a SecretVar-backed key.
        providers = {
          # Codex/OpenAI cache prompts automatically. This opts the provider into
          # Bifrost's cache policy without inventing a wire boolean; caller-supplied
          # prompt_cache_key/retention/options remain authoritative.
          codex.prompt_cache = { auto_inject = true; };
        } // lib.optionalAttrs (cfg.semanticCacheModalApp != null || cfg.headroomModalApp != null) {
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
              # Let the two-second remote deadline and bounded cancellation
              # release the shared slot before compression starts.
              default_request_timeout_in_seconds = 3;
              max_retries = 0;
            };
          };
        };
        plugins = [ {
          name = "semantic_cache";
          enabled = true;
          config = {
            provider = if cfg.semanticCacheModalApp == null && cfg.headroomModalApp == null then "" else "headroom_embeddings";
            embedding_model = "headroom-minilm-v1";
            dimension = if cfg.semanticCacheModalApp == null && cfg.headroomModalApp == null then 1 else 384;
            threshold = 0.98;
            ttl = "5m";
            default_cache_key = "deployment-v1";
            scope_by_virtual_key = true;
            vector_store_namespace = if cfg.semanticCacheModalApp == null && cfg.headroomModalApp == null then "BifrostScopedCacheV1" else "BifrostMiniLMCacheV1";
          };
        } {
          name = "headroom";
          path = "${config.services.bifrost.package}/lib/headroom.so";
          enabled = true;
          placement = "post_builtin";
          config = {
            enabled = cfg.headroomModalApp != null;
            embedding_proxy_enabled = cfg.semanticCacheModalApp != null || cfg.headroomModalApp != null;
            modal_app = if cfg.headroomModalApp != null then cfg.headroomModalApp else if cfg.semanticCacheModalApp != null then cfg.semanticCacheModalApp else "";
            modal_environment = "main";
            scope = "gateway";
            ccr = false;
            scope_key_env = "HEADROOM_SCOPE_KEY";
            modal_key_env = "HEADROOM_MODAL_TOKEN_ID";
            modal_secret_env = "HEADROOM_MODAL_TOKEN_SECRET";
            failure_policy = "open";
            # Scale-to-zero can require a fresh model load, not only restoration.
            timeout_ms = 30000;
            # Initial cost policy: bypass small results before waking Modal.
            min_text_bytes = 16384;
            cost_ledger_path = "${config.services.bifrost.stateDir}/headroom-budget.json";
            metrics_address = if cfg.semanticCacheModalApp == null && cfg.headroomModalApp == null then "" else "127.0.0.1:9909";
            metrics_token_env = "HEADROOM_METRICS_TOKEN";
            retention_seconds = 900;
          };
        } ];
      };
    };
    systemd.services.bifrost = {
      after = [ "network-online.target" "bifrost-valkey.service" ];
      wants = [ "network-online.target" ];
      requires = [ "bifrost-valkey.service" ];
      # Modal reads a config file even with explicit credentials. System services
      # need no home directory or ambient CLI profile; credentials come from env.
      environment = lib.optionalAttrs (cfg.semanticCacheModalApp != null || cfg.headroomModalApp != null) {
        MODAL_CONFIG_PATH = "/dev/null";
      };
      preStart = lib.mkBefore (''
        for name in NEON_HOST NEON_USER NEON_PASSWORD NEON_DATABASE BIFROST_ENCRYPTION_KEY BIFROST_ADMIN_PASSWORD; do
          if [ -z "''${!name:-}" ]; then echo "Required deployment setting missing" >&2; exit 1; fi
        done
        [[ "$NEON_HOST" == *-pooler.*.neon.tech ]] || exit 1
        [[ ''${#BIFROST_ENCRYPTION_KEY} -ge 32 && ''${#BIFROST_ADMIN_PASSWORD} -ge 32 ]] || exit 1
      '' + lib.optionalString (cfg.semanticCacheModalApp != null || cfg.headroomModalApp != null) ''
        for name in HEADROOM_MODAL_TOKEN_ID HEADROOM_MODAL_TOKEN_SECRET HEADROOM_SCOPE_KEY HEADROOM_METRICS_TOKEN; do
          if [ -z "''${!name:-}" ]; then echo "Required Headroom setting missing" >&2; exit 1; fi
        done
        [[ ''${#HEADROOM_SCOPE_KEY} -ge 32 && ''${#HEADROOM_METRICS_TOKEN} -ge 32 ]] || exit 1
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

    systemd.services.bifrost-valkey = {
      description = "Bifrost Valkey Search cache";
      wantedBy = [ "multi-user.target" ];
      preStart = ''
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
        supervised systemd
        loadmodule ${valkeySearch}/lib/valkey/modules/libsearch.so
        save ""
        appendonly no
        maxmemory 2gb
        maxmemory-policy allkeys-lfu
        EOF
        } > /run/bifrost-valkey/valkey.conf
      '';
      serviceConfig = {
        Type = "notify";
        ExecStart = "${pkgs.valkey}/bin/valkey-server /run/bifrost-valkey/valkey.conf";
        LoadCredential = [ "valkey-password:${cfg.redisPasswordFile}" ];
        DynamicUser = true;
        RuntimeDirectory = "bifrost-valkey";
        RuntimeDirectoryMode = "0700";
        WorkingDirectory = "/run/bifrost-valkey";
        UMask = "0077";
        Restart = "on-failure";
        RestartSec = 2;
        TimeoutStartSec = 60;
        TimeoutStopSec = 30;
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictSUIDSGID = true;
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        CapabilityBoundingSet = [];
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

    # A stable TailVIP/DNS identity, independent of the Oracle node hostname.
    # Service endpoints are tailnet-only; Funnel below uses the node identity.
    systemd.services.bifrost-inference-serve = {
      description = "Tailnet-only ai dashboard and inference";
      wantedBy = [ "multi-user.target" ];
      after = [ "tailscaled.service" "caddy.service" "bifrost.service" ];
      requires = [ "tailscaled.service" "caddy.service" "bifrost.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStartPre = "${pkgs.tailscale}/bin/tailscale wait --timeout=60s";
        ExecStart = "${pkgs.tailscale}/bin/tailscale serve --service=svc:ai --https=443 http://127.0.0.1:8082";
        ExecStop = "${pkgs.tailscale}/bin/tailscale serve --service=svc:ai --https=443 off";
      };
    };
    systemd.services.bifrost-admin-serve = {
      description = "Tailnet-only Bifrost administration";
      wantedBy = [ "multi-user.target" ];
      after = [ "tailscaled.service" "caddy.service" "bifrost-inference-serve.service" ];
      requires = [ "tailscaled.service" "caddy.service" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStartPre = "${pkgs.tailscale}/bin/tailscale wait --timeout=60s";
        ExecStart = "${pkgs.tailscale}/bin/tailscale serve --service=svc:ai --https=8443 http://127.0.0.1:8082";
        ExecStop = "${pkgs.tailscale}/bin/tailscale serve --service=svc:ai --https=8443 off";
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

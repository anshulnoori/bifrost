{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) types;

  cfg = config.services.bifrost;
  settingsFormat = pkgs.formats.json { };
in
{
  options = {
    services.bifrost = {
      enable = lib.mkEnableOption "Bifrost HTTP gateway";

      package = lib.mkPackageOption pkgs "bifrost-http" { };

      stateDir = lib.mkOption {
        type = types.path;
        default = "/var/lib/bifrost";
        example = "/var/lib/bifrost";
        description = "Application data directory (contains config.json and logs).";
      };

      host = lib.mkOption {
        type = types.str;
        default = "127.0.0.1";
        example = "0.0.0.0";
        description = "The host address which the Bifrost HTTP server listens to.";
      };

      port = lib.mkOption {
        type = types.port;
        default = 8080;
        example = 11111;
        description = "Which port the Bifrost HTTP server listens to.";
      };

      logLevel = lib.mkOption {
        type = types.enum [
          "debug"
          "info"
          "warn"
          "error"
        ];
        default = "info";
        description = "Logger level.";
      };

      logStyle = lib.mkOption {
        type = types.enum [
          "json"
          "pretty"
        ];
        default = "json";
        description = "Logger output style.";
      };

      settings = lib.mkOption {
        type = types.nullOr settingsFormat.type;
        default = null;
        description = ''
          Optional content for `config.json` under `services.bifrost.stateDir`.

          If set, the file will be written on service start (overwriting any existing config.json).
          If null, Bifrost will use an existing config.json (or bootstrap from environment).
        '';
        example = {
          providers = [
            {
              name = "openai";
              enabled = true;
              keys = [
                {
                  name = "default";
                  value = "env.OPENAI_API_KEY";
                }
              ];
            }
          ];
        };
      };

      environment = lib.mkOption {
        type = types.attrsOf types.str;
        default = { };
        description = "Extra environment variables for Bifrost.";
        example = {
          OPENAI_API_KEY = "...";
        };
      };

      environmentFile = lib.mkOption {
        description = ''
          Environment file to be passed to the systemd service.
          Useful for passing secrets to the service to prevent them from being
          world-readable in the Nix store.
        '';
        type = lib.types.nullOr lib.types.path;
        default = null;
        example = "/var/lib/secrets/bifrost.env";
      };

      claude = {
        enable = lib.mkEnableOption "the owner-scoped pinned Claude bridge";
        environmentFile = lib.mkOption {
          type = types.str;
          default = "";
          description = "Runtime file containing CLAUDE_BRIDGE_TOKEN, also supplied to Bifrost. Never put credentials in the Nix store.";
        };
      };

      openFirewall = lib.mkOption {
        type = types.bool;
        default = false;
        description = ''
          Whether to open the firewall for Bifrost.
          This adds `services.bifrost.port` to `networking.firewall.allowedTCPPorts`.
        '';
      };

      extraArgs = lib.mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "Extra CLI arguments passed to bifrost-http.";
        example = [
          "-some-flag"
          "value"
        ];
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions =
      let
        stateDirStr = toString cfg.stateDir;
      in
      [
        {
          assertion = builtins.dirOf stateDirStr == "/var/lib";
          message = "services.bifrost.stateDir must be a direct child of /var/lib (e.g. /var/lib/bifrost) to work with systemd StateDirectory and DynamicUser.";
        }
      ] ++ lib.optional cfg.claude.enable {
        assertion = lib.hasPrefix "/" cfg.claude.environmentFile && !lib.hasPrefix "/nix/store/" cfg.claude.environmentFile;
        message = "Claude requires an absolute runtime environmentFile outside the Nix store.";
      };

    programs.nix-ld.enable = lib.mkIf cfg.claude.enable true;

    users.groups.bifrost-claude = lib.mkIf cfg.claude.enable {};
    users.users.bifrost-claude = lib.mkIf cfg.claude.enable {
      isSystemUser = true;
      group = "bifrost-claude";
      home = "/var/lib/bifrost-claude";
    };
    systemd.services.bifrost-claude = lib.mkIf cfg.claude.enable {
      description = "Claude native Messages bridge (owner account only)";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      environment = {
        CLAUDE_CONFIG_DIR = "/var/lib/bifrost-claude";
        HOME = "/var/lib/bifrost-claude";
        SSL_CERT_FILE = "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt";
        NIX_LD = config.environment.sessionVariables.NIX_LD;
        NIX_LD_LIBRARY_PATH = config.environment.sessionVariables.NIX_LD_LIBRARY_PATH;
      };
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/bifrost-claude";
        EnvironmentFile = cfg.claude.environmentFile;
        User = "bifrost-claude";
        Group = "bifrost-claude";
        StateDirectory = "bifrost-claude";
        StateDirectoryMode = "0700";
        WorkingDirectory = "/var/lib/bifrost-claude";
        UMask = "0077";
        Restart = "on-failure";
        RestartSec = 5;
        TimeoutStopSec = 45;
        PrivateTmp = true;
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateDevices = true;
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        LimitCORE = 0;
        StandardOutput = "null";
        StandardError = "null";
      };
    };

    systemd.services.bifrost =
      let
        stateDirStr = toString cfg.stateDir;
        stateDirName = builtins.baseNameOf stateDirStr;
        configFile =
          if cfg.settings == null then null else settingsFormat.generate "bifrost-config.json" cfg.settings;
      in
      {
        description = "Bifrost AI Gateway (bifrost-http)";
        wantedBy = [ "multi-user.target" ];
        after = [ "network.target" ];

        environment = cfg.environment;

        preStart = lib.optionalString (configFile != null) ''
          install -Dm600 "${configFile}" "${stateDirStr}/config.json"
        '';

        serviceConfig = {
          ExecStart = lib.concatStringsSep " " (
            [
              (lib.getExe cfg.package)
              "-host"
              cfg.host
              "-port"
              (toString cfg.port)
              "-app-dir"
              stateDirStr
              "-log-level"
              cfg.logLevel
              "-log-style"
              cfg.logStyle
            ]
            ++ cfg.extraArgs
          );

          EnvironmentFile = lib.optional (cfg.environmentFile != null) cfg.environmentFile;

          WorkingDirectory = cfg.stateDir;
          StateDirectory = stateDirName;
          RuntimeDirectory = "bifrost";
          RuntimeDirectoryMode = "0755";

          PrivateTmp = true;
          DynamicUser = true;
          DevicePolicy = "closed";
          LockPersonality = true;
          PrivateUsers = true;
          ProtectHome = true;
          ProtectHostname = true;
          ProtectKernelLogs = true;
          ProtectKernelModules = true;
          ProtectKernelTunables = true;
          ProtectControlGroups = true;
          RestrictNamespaces = true;
          RestrictRealtime = true;
          SystemCallArchitectures = "native";
          UMask = "0077";
          RestrictAddressFamilies = [
            "AF_INET"
            "AF_INET6"
            "AF_UNIX"
          ];
          ProtectClock = true;
          ProtectProc = "invisible";
        };
      };

    networking.firewall = lib.mkIf cfg.openFirewall { allowedTCPPorts = [ cfg.port ]; };
  };

  meta.maintainers = [
    {
      name = "ReStranger";
      github = "ReStranger";
    }
  ];
}

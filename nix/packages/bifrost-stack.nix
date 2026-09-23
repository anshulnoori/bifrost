{ pkgs, src, bifrost-ui }:
pkgs.buildGoModule.override { go = pkgs.go_1_27; } {
  pname = "bifrost-stack";
  version = "1.4.9";
  inherit src;
  vendorHash = "sha256-lIZp8hMoa43fB/TkSM0iMA9jndvfRDkX6RM/0sRkeLc=";
  # go.work is an ignored developer file, so reconstruct it from tracked modules.
  postPatch = ''
    go work init ./core ./framework ./transports ./integrations/headroom ./deploy/cloudflare/container
    for module in plugins/*; do
      if [ -f "$module/go.mod" ]; then go work use "$module"; fi
    done
  '';
  # Vendor the workspace once: gateway and native plugin must share their ABI.
  modBuildPhase = ''
    GOFLAGS= go work vendor
  '';
  env.CGO_ENABLED = "1";
  doCheck = false; # Tests run separately, without provider credentials.
  buildPhase = ''
    runHook preBuild
    cp -r --no-preserve=mode ${bifrost-ui}/ui/. transports/bifrost-http/ui/
    mkdir -p $out/bin $out/lib
    go build -o $out/bin/bifrost-http ./transports/bifrost-http
    go build -buildmode=plugin -o $out/lib/headroom.so ./integrations/headroom
    go build -o $out/bin/bifrost-migrate ./deploy/cloudflare/container
    runHook postBuild
  '';
  installPhase = "true";
  meta.mainProgram = "bifrost-http";
}

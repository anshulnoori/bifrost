{
  pkgs,
  src,
  version,
}:
pkgs.buildNpmPackage {
  pname = "bifrost-ui";
  inherit version;
  inherit src;
  sourceRoot = "source/ui";

  npmDepsHash = "sha256-cOswnT4ZahWX66h9oiw4t3r5GZeOH/yjbnTCAsjVgnw=";

  # Vite builds offline. Do not replace the router layout with an old Next shell.
  # Avoid the build script's copy step (writes outside $PWD).
  npmBuildScript = "build-enterprise";

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/ui"
    cp -R --no-preserve=mode,ownership,timestamps out/. "$out/ui/"

    runHook postInstall
  '';
}

{
  lib,
  stdenv,
  fetchFromGitHub,
  cmake,
  ninja,
  pkg-config,
  git,
  grpc,
  protobuf,
  abseil-cpp,
  gtest,
  openssl,
  zlib,
  c-ares,
  re2,
  runCommand,
  valkey,
  binutils,
}: let
  staticLibrary = package:
    package.overrideAttrs (previous: {
      cmakeFlags = (previous.cmakeFlags or []) ++ ["-DBUILD_SHARED_LIBS=OFF" "-DCMAKE_POSITION_INDEPENDENT_CODE=ON"];
    });
  staticGtest = staticLibrary gtest;
  staticAbseil = staticLibrary (abseil-cpp.overrideAttrs (_: {
    buildInputs = [staticGtest];
  }));
  staticProtobuf = staticLibrary ((protobuf.override {
      abseil-cpp = staticAbseil;
      gtest = staticGtest;
      enableShared = false;
    }).overrideAttrs (_: {
      doCheck = false;
    }));
  staticRe2 = staticLibrary ((re2.override {abseil-cpp = staticAbseil;}).overrideAttrs (previous: {
    cmakeFlags = previous.cmakeFlags ++ ["-DRE2_USE_ICU=OFF" "-DRE2_BUILD_TESTING=OFF"];
    propagatedBuildInputs = [staticAbseil];
    doCheck = false;
  }));
  staticGrpc = staticLibrary (grpc.override {
    abseil-cpp = staticAbseil;
    protobuf = staticProtobuf;
    re2 = staticRe2;
  });
  src = fetchFromGitHub {
    owner = "valkey-io";
    repo = "valkey-search";
    rev = "788e8c90063abab69d0d3fff32206f8491c633ca";
    hash = "sha256-yRMDAB+gn8IqHJSlJJ2Yr9bKvDx2Q8MCGBdhQHsZqts=";
  };
  icu = stdenv.mkDerivation {
    pname = "valkey-search-icu";
    version = "76.1";
    inherit src;
    sourceRoot = "source/third_party/icu/source";
    configureFlags = [
      "--enable-static"
      "--disable-shared"
      "--with-data-packaging=static"
      "--disable-extras"
      "--disable-icuio"
      "--disable-layout"
      "--disable-tests"
      "--disable-samples"
      "--enable-tools"
    ];
    CFLAGS = "-O2 -fPIC";
    CXXFLAGS = "-O2 -fPIC";
    makeFlags = ["PKGDATA_MODE=static"];
    enableParallelBuilding = true;
  };
  highwayhash = stdenv.mkDerivation {
    pname = "highwayhash";
    version = "unstable-2024-04-18";
    src = fetchFromGitHub {
      owner = "google";
      repo = "highwayhash";
      rev = "f8381f3331d9c56a9792f9b4a35f61c41108c39e";
      hash = "sha256-h1zZChOPTHp1mYIt5UOUKyze8hS4kOTZ1GUtb2yPKIQ=";
    };
    nativeBuildInputs = [cmake ninja];
    cmakeFlags = ["-DBUILD_SHARED_LIBS=OFF" "-DCMAKE_POSITION_INDEPENDENT_CODE=ON"];
    ninjaFlags = ["highwayhash"];
    installPhase = ''
      install -Dm644 libhighwayhash.a "$out/lib/libhighwayhash.a"
      mkdir -p "$out/include/highwayhash"
      cp ../highwayhash/*.h "$out/include/highwayhash/"
    '';
  };
in
  stdenv.mkDerivation (finalAttrs: {
    pname = "valkey-search";
    version = "1.2.1";
    inherit src;
    nativeBuildInputs = [cmake ninja pkg-config git staticGrpc staticProtobuf binutils];
    buildInputs = [staticGrpc staticProtobuf staticAbseil staticGtest openssl zlib c-ares staticRe2 highwayhash];
    postPatch = ''
            substituteInPlace vmsdk/versionscript.lds \
              --replace-fail 'global:
        *;
      local:' 'global:
        ValkeyModule_OnLoad;
        ValkeyModule_OnUnload;
      local:
        *;'
            substituteInPlace submodules/CMakeLists.txt \
              --replace-fail 'file(READ "/etc/os-release" OS_RELEASE)' 'set(OS_RELEASE "NAME=NixOS")'
            substituteInPlace cmake/Modules/linux_utils.cmake \
              --replace-fail 'target_link_libraries(absl::all INTERFACE ''${ABSL_TARGET})' \
                'if(TARGET ''${ABSL_TARGET})
                  target_link_libraries(absl::all INTERFACE ''${ABSL_TARGET})
                endif()'
    '';
    preConfigure = ''
      mkdir -p build/icu
      ln -s ${icu} build/icu/install
    '';
    cmakeFlags = [
      "-DCMAKE_POLICY_VERSION_MINIMUM=3.5"
      "-DBUILD_UNIT_TESTS=OFF"
      "-DWITH_SUBMODULES_SYSTEM=ON"
      "-DCMAKE_SHARED_LINKER_FLAGS=-static-libstdc++ -Wl,--exclude-libs,ALL"
    ];
    ninjaFlags = ["libsearch"];
    installPhase = ''
      install -Dm755 libsearch.so "$out/lib/valkey/modules/libsearch.so"
      install -Dm644 ../LICENSE "$out/share/licenses/valkey-search/LICENSE"
      if readelf -d "$out/lib/valkey/modules/libsearch.so" | grep NEEDED | grep -E 'lib(absl|grpc|protobuf|stdc\+\+|re2|gtest)'; then
        exit 1
      fi
    '';
    passthru.tests.native =
      runCommand "valkey-search-native-test" {
        nativeBuildInputs = [valkey];
      } ''
        socket="$TMPDIR/valkey.sock"
        trap 'status=$?; valkey-cli -s "$socket" shutdown nosave >/dev/null 2>&1 || true; if test "$status" -ne 0; then cat "$TMPDIR/valkey.log"; fi' EXIT
        valkey-server --port 0 --unixsocket "$socket" --save "" --appendonly no \
          --daemonize yes --pidfile "$TMPDIR/valkey.pid" --logfile "$TMPDIR/valkey.log" \
          --loadmodule ${finalAttrs.finalPackage}/lib/valkey/modules/libsearch.so
        for attempt in {1..30}; do
          if valkey-cli -s "$socket" ping >/dev/null 2>&1; then break; fi
          sleep 1
        done
        test "$(valkey-cli -s "$socket" -e FT.CREATE smoke ON HASH PREFIX 1 doc: SCHEMA name TEXT embedding VECTOR FLAT 6 TYPE FLOAT32 DIM 2 DISTANCE_METRIC L2)" = OK
        test "$(valkey-cli -s "$socket" -e FT.CREATE smoke-cosine ON HASH PREFIX 1 doc: SCHEMA embedding VECTOR HNSW 6 TYPE FLOAT32 DIM 2 DISTANCE_METRIC COSINE)" = OK
        test "$(valkey-cli -s "$socket" -e HSET doc:1 name nativecheck)" = 1
        for attempt in {1..30}; do
          valkey-cli -s "$socket" --raw -e FT.SEARCH smoke '@name:nativecheck' NOCONTENT > result
          if test "$(head -n1 result)" = 1; then break; fi
          sleep 1
        done
        printf '1\ndoc:1\n' > expected
        diff -u expected result
        printf '\000\000\200\077\000\000\200\077' | valkey-cli -s "$socket" -e -x HSET doc:1 embedding
        printf '\000\000\200\100\000\000\100\100' | valkey-cli -s "$socket" -e -x HSET doc:2 embedding
        printf '1\ndoc:2\n' > expected
        for attempt in {1..30}; do
          printf '\000\000\000\100\000\000\200\100' | \
            valkey-cli -s "$socket" --raw -e -x FT.SEARCH smoke '*=>[KNN 1 @embedding $query]' NOCONTENT DIALECT 2 PARAMS 2 query > result
          if cmp -s expected result; then break; fi
          sleep 1
        done
        diff -u expected result
        printf '1\ndoc:1\n' > expected
        for attempt in {1..30}; do
          printf '\000\000\000\100\000\000\200\100' | \
            valkey-cli -s "$socket" --raw -e -x FT.SEARCH smoke-cosine '*=>[KNN 1 @embedding $query]' NOCONTENT DIALECT 2 PARAMS 2 query > result
          if cmp -s expected result; then break; fi
          sleep 1
        done
        diff -u expected result
        touch "$out"
      '';
    meta = {
      license = lib.licenses.bsd3;
      platforms = lib.platforms.linux;
    };
  })

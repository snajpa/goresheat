{ lib, buildGoModule }:                                                         
buildGoModule {                                                                
  name = "goresheat";
  vendorHash = "sha256-iVGS9bvZ01AKuaFt1XLOKp6gW1NnPYTk0LoZzjsNmTg=";
  src = ./.;
}
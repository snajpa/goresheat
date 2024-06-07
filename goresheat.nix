{ lib, buildGoModule }:                                                         
buildGoModule {                                                                
  name = "goresheat";
  vendorHash = "sha256-e4w0YHxa/ImM3DseWCugbuytn5TN666IO69Dl7B0vpc=";
  src = ./.;
}
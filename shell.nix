{ pkgs ? import <nixpkgs> {} }:

with builtins;
let
  goresheat = pkgs.callPackage ./goresheat.nix {};
in
  pkgs.mkShell {
    name = "goresheat";
    buildInputs = [
      goresheat
    ];
  }
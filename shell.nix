{ pkgs ? import <nixpkgs> {} }:
pkgs.mkShell {
  nativeBuildInputs = [ pkgs.pkg-config pkgs.go pkgs.nodejs pkgs.just ];
  buildInputs = [ pkgs.gtk3 pkgs.webkitgtk_4_1 ];
}

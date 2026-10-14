let
  sources = import ./npins;
  pkgs = import sources.nixpkgs { };
in
pkgs.mkShellNoCC {
  packages = with pkgs; [
    go
    gopls
  ];

  # Pure-Go static binary, no C toolchain needed.
  CGO_ENABLED = "0";
}

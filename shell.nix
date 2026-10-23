{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = with pkgs; [
    go_1_26
    just
  ];

  # Keep Go from trying to download a different toolchain outside Nix.
  env.GOTOOLCHAIN = "local";

  # nix-shell's generated TMPDIR path is long enough to overflow Linux's
  # Unix-socket path limit in tests that create sockets below t.TempDir().
  shellHook = ''
    export TMPDIR=/tmp
  '';
}
